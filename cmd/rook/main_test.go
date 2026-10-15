package main

import (
	"context"
	"github.com/phpsandbox/rook/internal/host"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phpsandbox/rook/internal/agent"
	"github.com/vmihailenco/msgpack/v5"
	"nhooyr.io/websocket"
)

func TestAgentPlaneURLUsesWebSocketScheme(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		controlPlane string
		expected     string
	}{
		"HTTP":              {controlPlane: "http://rook.phpsandbox.test/connect", expected: "ws://rook.phpsandbox.test/connect?channel=control&server_id=server-1"},
		"HTTPS":             {controlPlane: "https://rook.phpsandbox.io/connect", expected: "wss://rook.phpsandbox.io/connect?channel=control&server_id=server-1"},
		"WS":                {controlPlane: "ws://rook.phpsandbox.test/connect", expected: "ws://rook.phpsandbox.test/connect?channel=control&server_id=server-1"},
		"Configured server": {controlPlane: "wss://rook.phpsandbox.io/connect?server_id=other", expected: "wss://rook.phpsandbox.io/connect?channel=control&server_id=server-1"},
		"WSS":               {controlPlane: "wss://rook.phpsandbox.io/connect", expected: "wss://rook.phpsandbox.io/connect?channel=control&server_id=server-1"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			actual := agentPlaneURL(test.controlPlane, "server-1", "control")
			if actual != test.expected {
				t.Fatalf("agentPlaneURL() = %q, want %q", actual, test.expected)
			}
		})
	}
}

func TestDataReconnectCancelsActiveAndQueuedHTTPRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entered := make(chan struct{}, 65)
	canceled := make(chan struct{}, 65)
	var requests atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			_, _ = w.Write([]byte("ok"))
			return
		}
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		entered <- struct{}{}
		<-r.Context().Done()
		canceled <- struct{}{}
	}))
	defer origin.Close()
	parsed, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	bindings := host.NewBindings(t.TempDir())
	if err := bindings.Set("app", host.Binding{Port: port}); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan *websocket.Conn, 2)
	plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err == nil {
			accepted <- conn
		}
	}))
	defer plane.Close()
	client := agent.NewWSClient("ws"+strings.TrimPrefix(plane.URL, "http"), "test", true)
	if err := client.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	first := <-accepted
	defer first.CloseNow()
	relay := agent.NewRelayManager(agent.NewProxy(bindings), client)
	done := make(chan struct{})
	go func() { runRelayLoop(ctx, client, relay, nil); close(done) }()
	defer func() { cancel(); client.Close(); <-done }()
	writeFrame := func(conn *websocket.Conn, id, path string) {
		t.Helper()
		data, err := msgpack.Marshal(agent.RelayFrame{Protocol: agent.RelayProtocol, Type: agent.RelayFrameOpen, Kind: agent.RelayKindHTTP, StreamID: id, DeploymentID: "app", Method: http.MethodGet, Path: path})
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Write(ctx, websocket.MessageBinary, data); err != nil {
			t.Fatal(err)
		}
	}
	// Fill all 64 slots and leave one bodyless request waiting for admission.
	for i := 0; i < 65; i++ {
		writeFrame(first, strconv.Itoa(i), "/slow")
	}
	for i := 0; i < 64; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("upstream requests did not start")
		}
	}
	first.CloseNow()
	var replacement *websocket.Conn
	select {
	case replacement = <-accepted:
	case <-ctx.Done():
		t.Fatal("data plane did not reconnect")
	}
	defer replacement.CloseNow()
	for i := 0; i < 64; i++ {
		select {
		case <-canceled:
		case <-ctx.Done():
			t.Fatal("old HTTP requests were not canceled")
		}
	}
	writeFrame(replacement, "fresh", "/ok")
	var body []byte
	for {
		_, data, err := replacement.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var frame agent.RelayFrame
		if err := msgpack.Unmarshal(data, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.StreamID != "fresh" {
			t.Fatalf("stale stream reached replacement connection: %#v", frame)
		}
		if frame.Type == agent.RelayFrameReset {
			t.Fatalf("fresh request failed: %#v", frame)
		}
		body = append(body, frame.Data...)
		if frame.Type == agent.RelayFrameEnd {
			break
		}
	}
	if string(body) != "ok" {
		t.Fatalf("body=%q", body)
	}
	// The waiting request can win a released slot while Reset cancels the
	// other streams. If it starts in that race, it must also be canceled.
	for i := 64; i < int(requests.Load()); i++ {
		select {
		case <-canceled:
		case <-ctx.Done():
			t.Fatal("request admitted during reset was not canceled")
		}
	}
}

func TestCorruptOperationStateNeverConnects(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) }))
	defer server.Close()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "operations"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "operations", "invalid.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := run(ctx, agent.Config{ServerID: "test", Token: "test", ControlPlane: server.URL, StateDir: dir}, false)
	if err == nil || !strings.Contains(err.Error(), "recover operations") || requests.Load() != 0 {
		t.Fatalf("corrupt host connected: %v, requests=%d", err, requests.Load())
	}
}

func TestSlowLogDeliveryDoesNotDelayDurableTerminalRecord(t *testing.T) {
	executor := &host.Executor{Directory: t.TempDir()}
	blocked := make(chan struct{})
	defer close(blocked)
	done := make(chan host.Result, 1)
	go func() {
		result, err := executeHostCommand(context.Background(), executor, host.Request{
			ID: "no-reader", Action: "exec", Command: []string{"sh", "-c", "printf output"},
		}, func(agent.OutboundMessage) { <-blocked })
		if err != nil {
			t.Error(err)
		}
		done <- result
	}()
	select {
	case result := <-done:
		saved, err := executor.Status("no-reader")
		if err != nil || saved.Status != "completed" || result.Output != "output" {
			t.Fatalf("result=%+v saved=%+v error=%v", result, saved, err)
		}
	case <-time.After(time.Second):
		t.Fatal("log delivery prevented terminal record")
	}
}
