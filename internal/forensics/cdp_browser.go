package forensics

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
)

var devtoolsListeningRe = regexp.MustCompile(`DevTools listening on (ws://\S+)`)

// FindBrowser locates a Chrome or Chromium executable. If explicit is non-empty,
// it validates that path directly without falling back to system search.
func FindBrowser(explicit string) (string, error) {
	if explicit != "" {
		if info, err := os.Stat(explicit); err != nil || info.IsDir() {
			return "", fmt.Errorf("chrome binary %q not usable", explicit)
		}
		return explicit, nil
	}
	if envBin := strings.TrimSpace(os.Getenv("LEMMINGS_CHROME_BIN")); envBin != "" {
		if info, err := os.Stat(envBin); err == nil && !info.IsDir() {
			return envBin, nil
		}
	}

	candidates := browserCandidates(runtime.GOOS)
	for _, c := range candidates {
		if filepath.IsAbs(c) {
			if info, err := os.Stat(c); err == nil && !info.IsDir() {
				return c, nil
			}
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no Chrome or Chromium binary found")
}

func browserCandidates(goos string) []string {
	switch goos {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"google-chrome",
			"chromium",
		}
	case "windows":
		return []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
			"chrome.exe",
			"msedge.exe",
		}
	default:
		return []string{
			"chromium",
			"chromium-browser",
			"google-chrome",
			"google-chrome-stable",
			"brave-browser",
		}
	}
}

// minimalBrowserEnv builds a strictly allowlisted environment for the spawned
// Chrome process so ambient host secrets (vault tokens, AWS credentials, SMTP
// passwords) are NEVER inherited by the browser or its renderer processes (SEC-09/S5).
func minimalBrowserEnv(tmpDir string) []string {
	env := []string{
		"HOME=" + tmpDir,
		"TMPDIR=" + tmpDir,
		"LANG=en_US.UTF-8",
	}
	allowKeys := []string{"PATH"}
	switch runtime.GOOS {
	case "linux":
		allowKeys = append(allowKeys, "DISPLAY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS")
	case "windows":
		allowKeys = append(allowKeys, "SYSTEMROOT", "WINDIR", "COMSPEC")
		env = append(env, "USERPROFILE="+tmpDir, "LOCALAPPDATA="+tmpDir, "TEMP="+tmpDir, "TMP="+tmpDir)
	}
	for _, k := range allowKeys {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// browserInstance owns a CDP WebSocket connection and (when launched locally)
// the headless Chromium child process and its ephemeral 0700 temp directory.
type browserInstance struct {
	conn      *cdpConn
	desc      string
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	tmpDir    string
	drainDone chan struct{}
	closeOnce sync.Once
}

// localBrowserArgs returns the hardened Chromium command-line flags for an
// isolated headless instance. --use-mock-keychain and --password-store=basic
// prevent macOS SecurityAgent ("Keychain Not Found") and Linux GNOME Keyring /
// KWallet GUI prompts when HOME points to an ephemeral temp directory.
func localBrowserArgs(userDataDir string) []string {
	return []string{
		"--headless=new",
		"--remote-debugging-port=0",
		"--remote-debugging-address=127.0.0.1",
		"--remote-allow-origins=http://127.0.0.1",
		"--user-data-dir=" + userDataDir,
		"--use-mock-keychain",
		"--password-store=basic",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-sync",
		"--disable-default-apps",
		"--disable-translate",
		"--disable-gpu",
		"--mute-audio",
		"--hide-scrollbars",
		"--window-size=1280,800",
		"about:blank",
	}
}

func launchLocalBrowser(ctx context.Context, binPath string) (*browserInstance, error) {
	tmpDir, err := os.MkdirTemp("", "lemmings-cdp-*")
	if err != nil {
		return nil, fmt.Errorf("create cdp temp dir: %w", err)
	}
	if err := os.Chmod(tmpDir, 0o700); err != nil && runtime.GOOS != "windows" {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("chmod cdp temp dir: %w", err)
	}

	userDataDir := filepath.Join(tmpDir, "profile")
	if err := os.Mkdir(userDataDir, 0o700); err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("create cdp profile dir: %w", err)
	}

	procCtx, cancel := context.WithCancel(context.Background())
	args := localBrowserArgs(userDataDir)

	cmd := exec.CommandContext(procCtx, binPath, args...)
	cmd.Env = minimalBrowserEnv(tmpDir)
	cmd.Stdout = io.Discard
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("chrome stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("start chrome: %w", err)
	}

	wsURLCh := make(chan string, 1)
	errCh := make(chan error, 1)
	drainDone := make(chan struct{})

	go func() {
		defer close(drainDone)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 16<<10), 256<<10)
		found := false
		var lastLines []string
		for sc.Scan() {
			line := sc.Text()
			if !found {
				if m := devtoolsListeningRe.FindStringSubmatch(line); m != nil {
					found = true
					wsURLCh <- m[1]
				} else {
					if len(lastLines) >= 8 {
						lastLines = lastLines[1:]
					}
					lastLines = append(lastLines, line)
				}
			}
		}
		if !found {
			errCh <- fmt.Errorf("chrome exited before DevTools ready: %s", strings.Join(lastLines, " | "))
		}
	}()

	startCtx, startCancel := context.WithTimeout(ctx, 15*time.Second)
	defer startCancel()

	var wsURL string
	select {
	case wsURL = <-wsURLCh:
	case err := <-errCh:
		cancel()
		_ = cmd.Wait()
		<-drainDone
		_ = os.RemoveAll(tmpDir)
		return nil, err
	case <-startCtx.Done():
		cancel()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		<-drainDone
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("waiting for chrome DevTools endpoint: %w", startCtx.Err())
	}

	conn, err := dialCDP(startCtx, wsURL)
	if err != nil {
		cancel()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		<-drainDone
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}

	return &browserInstance{
		conn:      conn,
		desc:      filepath.Base(binPath),
		cmd:       cmd,
		cancel:    cancel,
		tmpDir:    tmpDir,
		drainDone: drainDone,
	}, nil
}

func connectRemoteBrowser(ctx context.Context, rawURL string, allowRemote bool) (*browserInstance, error) {
	wsURL := strings.TrimSpace(rawURL)
	parsed, err := url.Parse(wsURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https" && parsed.Scheme != "ws" && parsed.Scheme != "wss") {
		return nil, fmt.Errorf("cdp-url must start with ws://, wss://, http://, or https://")
	}
	if !allowRemote && !discovery.IsLoopbackURL(wsURL) {
		return nil, fmt.Errorf("remote cdp-url %q requires -allow-remote-cdp (loopback only by default)", discovery.SafeURL(wsURL))
	}

	if parsed.Scheme == "http" || parsed.Scheme == "https" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(wsURL, "/")+"/json/version", nil)
		if err != nil {
			return nil, fmt.Errorf("build cdp version request: %w", err)
		}
		tr := &http.Transport{
			DialContext:     discovery.SafeDialContext(&net.Dialer{Timeout: 10 * time.Second}),
			TLSClientConfig: discovery.MinTLSConfig(),
		}
		defer tr.CloseIdleConnections()
		client := &http.Client{
			Timeout:   5 * time.Second,
			Transport: tr,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("stopped after 5 redirects")
				}
				if !allowRemote && !discovery.IsLoopbackURL(req.URL.String()) {
					return fmt.Errorf("remote cdp redirect to %q requires -allow-remote-cdp (loopback only by default)", discovery.SafeURL(req.URL.String()))
				}
				return nil
			},
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("query remote cdp /json/version: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("remote cdp /json/version returned HTTP %d", resp.StatusCode)
		}
		var meta struct {
			WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&meta); err != nil {
			return nil, fmt.Errorf("decode remote cdp /json/version: %w", err)
		}
		if strings.TrimSpace(meta.WebSocketDebuggerURL) == "" {
			return nil, errors.New("remote cdp /json/version missing webSocketDebuggerUrl")
		}
		wsURL = strings.TrimSpace(meta.WebSocketDebuggerURL)
	}

	wsParsed, err := url.Parse(wsURL)
	if err != nil || (wsParsed.Scheme != "ws" && wsParsed.Scheme != "wss") {
		return nil, fmt.Errorf("cdp-url must resolve to a ws:// or wss:// endpoint")
	}
	if !allowRemote && !discovery.IsLoopbackURL(wsURL) {
		return nil, fmt.Errorf("remote cdp webSocketDebuggerUrl %q requires -allow-remote-cdp (loopback only by default)", discovery.SafeURL(wsURL))
	}

	conn, err := dialCDP(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	return &browserInstance{
		conn: conn,
		desc: "remote-cdp",
	}, nil
}

// Close shuts down the CDP connection, terminates any local Chrome child process,
// joins stderr draining, and deletes the ephemeral temp directory.
func (b *browserInstance) Close() error {
	var firstErr error
	b.closeOnce.Do(func() {
		if b.conn != nil {
			if b.cmd != nil {
				closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = b.conn.Call(closeCtx, "", "Browser.close", nil, nil)
				cancel()
			}
			firstErr = b.conn.Close()
		}
		if b.cancel != nil {
			b.cancel()
		}
		if b.cmd != nil {
			waitDone := make(chan struct{})
			go func() {
				_ = b.cmd.Wait()
				close(waitDone)
			}()
			select {
			case <-waitDone:
			case <-time.After(3 * time.Second):
				if b.cmd.Process != nil {
					_ = b.cmd.Process.Kill()
				}
				<-waitDone
			}
		}
		if b.drainDone != nil {
			<-b.drainDone
		}
		if b.tmpDir != "" {
			_ = os.RemoveAll(b.tmpDir)
		}
	})
	return firstErr
}
