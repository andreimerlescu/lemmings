package main

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/plan"
	"github.com/andreimerlescu/lemmings/v2/internal/swarm"
)

func TestArcadeAuthEndpoints(t *testing.T) {
	a := &arcade{chaos: 0, capacity: 0}
	srv := httptest.NewServer(a.routes())
	defer srv.Close()

	// 1. Unauthenticated /profile tells visitor to sign in.
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	resp, err := client.Get(srv.URL + "/profile")
	if err != nil {
		t.Fatal(err)
	}
	body := readAllStr(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "You must be signed in to perform this action.") {
		t.Fatalf("unauth /profile: status=%d body=%q", resp.StatusCode, body)
	}

	// 2. Missing credentials on POST /login return 401.
	resp, err = client.PostForm(srv.URL+"/login", url.Values{"username": {"player1"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = readAllStr(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing password status=%d, want 401", resp.StatusCode)
	}

	// 3. Form POST /login sets cookies + X-Arcade-Session and returns 200.
	resp, err = client.PostForm(srv.URL+"/login", url.Values{"username": {"player1"}, "password": {"secret"}})
	if err != nil {
		t.Fatal(err)
	}
	body = readAllStr(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Welcome player1!") {
		t.Fatalf("form POST /login: status=%d body=%q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Arcade-Session"); got != "arcade-player1" {
		t.Fatalf("X-Arcade-Session = %q, want arcade-player1", got)
	}

	// 4. Authenticated /profile greets user.
	resp, err = client.Get(srv.URL + "/profile")
	if err != nil {
		t.Fatal(err)
	}
	body = readAllStr(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Welcome player1!") {
		t.Fatalf("auth /profile: status=%d body=%q", resp.StatusCode, body)
	}

	// 5. GET /logout clears auth cookies and redirects 303 to /.
	noRedir := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err = noRedir.Get(srv.URL + "/logout")
	if err != nil {
		t.Fatal(err)
	}
	_ = readAllStr(t, resp)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("GET /logout: status=%d location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp, err = client.Get(srv.URL + "/profile")
	if err != nil {
		t.Fatal(err)
	}
	body = readAllStr(t, resp)
	if !strings.Contains(body, "You must be signed in to perform this action.") {
		t.Fatalf("/profile after logout should be unauthenticated: %q", body)
	}

	// 6. JSON POST /login also succeeds.
	jsonJar, _ := cookiejar.New(nil)
	jsonClient := &http.Client{Jar: jsonJar}
	resp, err = jsonClient.Post(srv.URL+"/login", "application/json", strings.NewReader(`{"username":"jsonuser","password":"pw"}`))
	if err != nil {
		t.Fatal(err)
	}
	body = readAllStr(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Welcome jsonuser!") || resp.Header.Get("X-Arcade-Session") != "arcade-jsonuser" {
		t.Fatalf("JSON POST /login: status=%d header=%q body=%q", resp.StatusCode, resp.Header.Get("X-Arcade-Session"), body)
	}

	// 7. POST /magic-unicorn-pixie-dust returns 404.
	resp, err = jsonClient.Post(srv.URL+"/magic-unicorn-pixie-dust", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = readAllStr(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST /magic-unicorn-pixie-dust status=%d, want 404", resp.StatusCode)
	}
}

func TestExamplePlansParseAndRunAgainstArcade(t *testing.T) {
	a := &arcade{chaos: 0, capacity: 0}
	srv := httptest.NewServer(a.routes())
	defer srv.Close()

	planFiles, err := filepath.Glob(filepath.Join("..", "plans", "*.yaml"))
	if err != nil || len(planFiles) != 5 {
		t.Fatalf("expected 5 example YAML plans, got %v (err=%v)", planFiles, err)
	}
	for _, f := range planFiles {
		if _, err := plan.LoadSuite(f, srv.URL+"/", plan.DefaultMaxPlanItems); err != nil {
			t.Errorf("plan %s failed LoadSuite: %v", f, err)
		}
	}

	// Verify neon-arcade-auth.yaml and neon-arcade-personas.yaml pass 100% against Neon Arcade (chaos=0).
	for _, name := range []string{"neon-arcade-auth.yaml", "neon-arcade-personas.yaml"} {
		t.Run(name, func(t *testing.T) {
			cfg := swarm.DefaultConfig()
			cfg.Hit = srv.URL + "/"
			cfg.Terrain = 1
			cfg.Pack = 10
			cfg.Limit = 10
			cfg.Until = 5 * time.Second
			cfg.Ramp = 20 * time.Millisecond
			cfg.ThinkMin = 1 * time.Millisecond
			cfg.ThinkMax = 5 * time.Millisecond
			cfg.PlanFile = filepath.Join("..", "plans", name)
			cfg.RogueRate = 0
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate %s: %v", name, err)
			}
			sw, err := swarm.New(context.Background(), cfg, swarm.Options{})
			if err != nil {
				t.Fatalf("swarm.New %s: %v", name, err)
			}
			if err := sw.Run(context.Background()); err != nil {
				t.Fatalf("swarm.Run %s: %v", name, err)
			}
			if passed, failed := sw.Metrics().PlanLemmingsPassed.Load(), sw.Metrics().PlanLemmingsFailed.Load(); passed != 10 || failed != 0 {
				t.Fatalf("%s: passed=%d failed=%d, want 10/0", name, passed, failed)
			}
		})
	}
}

func readAllStr(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return sb.String()
}
