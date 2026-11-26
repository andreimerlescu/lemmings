package forensics

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"golang.org/x/net/websocket"
)

const (
	cdpMaxFrameBytes    = 16 << 20 // 16 MiB cap for base64 PNG/JPEG screenshots
	cdpEventQueueBuffer = 256
)

type cdpRequest struct {
	ID        int64  `json:"id"`
	Method    string `json:"method"`
	Params    any    `json:"params,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

type cdpMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string {
	return fmt.Sprintf("cdp error %d: %s", e.Code, e.Message)
}

type cdpEvent struct {
	SessionID string
	Method    string
	Params    json.RawMessage
}

type cdpResp struct {
	result json.RawMessage
	err    error
}

// cdpConn multiplexes Chrome DevTools Protocol JSON-RPC calls and per-session
// event streams over a single WebSocket connection. Safe for concurrent use.
type cdpConn struct {
	ws        *websocket.Conn
	writeMu   sync.Mutex
	mu        sync.Mutex
	pending   map[int64]chan cdpResp
	listeners map[string]chan cdpEvent
	closed    bool
	closeErr  error
	nextID    atomic.Int64
	done      chan struct{}
	closeOnce sync.Once
}

func dialCDP(ctx context.Context, wsURL string) (*cdpConn, error) {
	cfg, err := websocket.NewConfig(wsURL, "http://127.0.0.1")
	if err != nil {
		return nil, fmt.Errorf("cdp config: %w", err)
	}
	if cfg.Location == nil || (cfg.Location.Scheme != "ws" && cfg.Location.Scheme != "wss") {
		return nil, fmt.Errorf("cdp dial: unsupported websocket url %q", discovery.SafeURL(wsURL))
	}
	host := cfg.Location.Hostname()
	if host == "" {
		return nil, fmt.Errorf("cdp dial: missing host in %q", discovery.SafeURL(wsURL))
	}
	port := cfg.Location.Port()
	if port == "" {
		if cfg.Location.Scheme == "wss" {
			port = "443"
		} else {
			port = "80"
		}
	}

	safeDial := discovery.SafeDialContext(&net.Dialer{Timeout: 10 * time.Second})
	rawConn, err := safeDial(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("cdp dial: %w", err)
	}

	var netConn net.Conn = rawConn
	if cfg.Location.Scheme == "wss" {
		tlsCfg := discovery.MinTLSConfig()
		tlsCfg.ServerName = host
		tlsConn := tls.Client(rawConn, tlsCfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("cdp tls handshake: %w", err)
		}
		cfg.TlsConfig = tlsCfg
		netConn = tlsConn
	}

	if dl, ok := ctx.Deadline(); ok {
		_ = netConn.SetDeadline(dl)
	}
	stopCancel := context.AfterFunc(ctx, func() {
		_ = netConn.SetDeadline(time.Now())
	})
	ws, err := websocket.NewClient(cfg, netConn)
	if !stopCancel() {
		if ws != nil {
			_ = ws.Close()
		} else {
			_ = netConn.Close()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("cdp handshake: %w", err)
	}
	_ = netConn.SetDeadline(time.Time{})
	ws.MaxPayloadBytes = cdpMaxFrameBytes

	c := &cdpConn{
		ws:        ws,
		pending:   make(map[int64]chan cdpResp),
		listeners: make(map[string]chan cdpEvent),
		done:      make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *cdpConn) readLoop() {
	defer close(c.done)
	for {
		var raw []byte
		if err := websocket.Message.Receive(c.ws, &raw); err != nil {
			c.failAll(err)
			return
		}
		var msg cdpMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if msg.ID != 0 {
			c.mu.Lock()
			ch := c.pending[msg.ID]
			delete(c.pending, msg.ID)
			c.mu.Unlock()
			if ch != nil {
				var err error
				if msg.Error != nil {
					err = msg.Error
				}
				ch <- cdpResp{result: msg.Result, err: err}
			}
			continue
		}
		if msg.Method != "" {
			c.mu.Lock()
			ch := c.listeners[msg.SessionID]
			c.mu.Unlock()
			if ch != nil {
				select {
				case ch <- cdpEvent{SessionID: msg.SessionID, Method: msg.Method, Params: msg.Params}:
				default:
					// Slow session drops excess events rather than blocking the reader.
				}
			}
		}
	}
}

func (c *cdpConn) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.closeErr = err
	for id, ch := range c.pending {
		ch <- cdpResp{err: err}
		delete(c.pending, id)
	}
	for sid, ch := range c.listeners {
		close(ch)
		delete(c.listeners, sid)
	}
}

// SubscribeSession registers an event channel for a flat CDP sessionID.
func (c *cdpConn) SubscribeSession(sessionID string) (<-chan cdpEvent, func()) {
	ch := make(chan cdpEvent, cdpEventQueueBuffer)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	c.listeners[sessionID] = ch
	c.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			c.mu.Lock()
			if cur, ok := c.listeners[sessionID]; ok && cur == ch {
				delete(c.listeners, sessionID)
			}
			c.mu.Unlock()
		})
	}
}

// Call invokes a CDP method on the browser (sessionID == "") or an attached target session.
func (c *cdpConn) Call(ctx context.Context, sessionID, method string, params, out any) error {
	id := c.nextID.Add(1)
	respCh := make(chan cdpResp, 1)

	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		c.mu.Unlock()
		if err == nil {
			err = errors.New("cdp connection closed")
		}
		return err
	}
	c.pending[id] = respCh
	c.mu.Unlock()

	payload, err := json.Marshal(cdpRequest{
		ID:        id,
		Method:    method,
		Params:    params,
		SessionID: sessionID,
	})
	if err != nil {
		c.deletePending(id)
		return fmt.Errorf("cdp marshal %s: %w", method, err)
	}

	c.writeMu.Lock()
	writeErr := websocket.Message.Send(c.ws, string(payload))
	c.writeMu.Unlock()
	if writeErr != nil {
		c.deletePending(id)
		return fmt.Errorf("cdp send %s: %w", method, writeErr)
	}

	select {
	case <-ctx.Done():
		c.deletePending(id)
		return ctx.Err()
	case r := <-respCh:
		if r.err != nil {
			return fmt.Errorf("cdp %s: %w", method, r.err)
		}
		if out != nil && len(r.result) > 0 {
			if err := json.Unmarshal(r.result, out); err != nil {
				return fmt.Errorf("cdp decode %s: %w", method, err)
			}
		}
		return nil
	}
}

func (c *cdpConn) deletePending(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// Close shuts down the WebSocket and joins readLoop. Idempotent.
func (c *cdpConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.ws.Close()
		<-c.done
	})
	return err
}
