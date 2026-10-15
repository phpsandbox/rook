package agent

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func waitRelayFrame(t *testing.T, sender *recordingTunnelSender, id, kind string) RelayFrame {
	t.Helper()
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		sender.mu.Lock()
		for _, frame := range sender.messages {
			if frame.StreamID == id && (frame.Type == kind || kind == "" && (frame.Type == RelayFrameEnd || frame.Type == RelayFrameReset)) {
				sender.mu.Unlock()
				return frame
			}
		}
		sender.mu.Unlock()
		select {
		case <-deadline:
			t.Fatalf("missing %s for %s", kind, id)
		case <-ticker.C:
		}
	}
}

func waitRelayWorkers(t *testing.T, manager *RelayManager, count int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		manager.mu.Lock()
		got := len(manager.workers)
		manager.mu.Unlock()
		if got == count {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("workers=%d, want %d", got, count)
		case <-ticker.C:
		}
	}
}

func relayOpen(id, deployment, path string, upgrade bool) RelayFrame {
	frame := RelayFrame{Protocol: RelayProtocol, Type: RelayFrameOpen, Kind: RelayKindHTTP, StreamID: id, DeploymentID: deployment, Method: http.MethodGet, Path: path}
	if upgrade {
		frame.Headers = []HeaderPair{{"Upgrade", "websocket"}}
	}
	return frame
}

func relayReset(id string) RelayFrame {
	return RelayFrame{Protocol: RelayProtocol, Type: RelayFrameReset, Kind: RelayKindHTTP, StreamID: id, Error: "caller canceled"}
}

func TestSlowWebSocketHandshakeDoesNotBlockOtherStreams(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(entered)
			<-release
			return
		}
		_, _ = io.WriteString(w, "healthy")
	}))
	defer origin.Close()
	defer close(release)
	sender := newRecordingTunnelSender()
	manager := NewRelayManager(proxyForTestServers(t, origin), sender)
	defer manager.Reset(context.Canceled)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	opened := make(chan struct{})
	go func() { manager.Handle(ctx, relayOpen("slow", "deployment-0", "/slow", true)); close(opened) }()
	select {
	case <-opened:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("upgrade blocked receiver")
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("handshake did not reach origin")
	}
	manager.Handle(ctx, relayOpen("healthy", "deployment-0", "/ok", false))
	frame := waitRelayFrame(t, sender, "healthy", RelayFrameData)
	if string(frame.Data) != "healthy" {
		t.Fatalf("response=%q", frame.Data)
	}
	manager.Handle(ctx, relayReset("slow"))
	waitRelayWorkers(t, manager, 0)
}

func TestAdmittedUploadOverflowDoesNotBlockOtherStreams(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/upload" {
			close(entered)
			<-release
			return
		}
		_, _ = io.WriteString(w, "healthy")
	}))
	defer origin.Close()
	defer close(release)
	sender := newRecordingTunnelSender()
	manager := NewRelayManager(proxyForTestServers(t, origin), sender)
	defer manager.Reset(context.Canceled)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	open := relayOpen("upload", "deployment-0", "/upload", false)
	open.Method = http.MethodPost
	open.HasBody = boolPtr(true)
	manager.Handle(ctx, open)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("upload did not reach origin")
	}
	handled := make(chan struct{})
	go func() {
		for i := 0; i < 512; i++ {
			manager.Handle(ctx, RelayFrame{Protocol: RelayProtocol, Type: RelayFrameData, Kind: RelayKindHTTP, StreamID: "upload", Data: make([]byte, relayChunkSize)})
			manager.mu.Lock()
			stream := manager.streams["upload"]
			manager.mu.Unlock()
			if stream == nil {
				break
			}
		}
		manager.Handle(ctx, relayReset("upload"))
		manager.Handle(ctx, relayOpen("healthy", "deployment-0", "/ok", false))
		close(handled)
	}()
	select {
	case <-handled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("admitted upload blocked shared receiver")
	}
	if frame := waitRelayFrame(t, sender, "upload", RelayFrameReset); frame.Error != errRelayBufferFull.Error() {
		t.Fatalf("reset=%+v", frame)
	}
	if frame := waitRelayFrame(t, sender, "healthy", RelayFrameData); string(frame.Data) != "healthy" {
		t.Fatalf("response=%q", frame.Data)
	}
	waitRelayWorkers(t, manager, 0)
}

func TestUpstreamWebSocketRedirectStaysWithinBinding(t *testing.T) {
	var requests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(403) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL+"/private", 302) }))
	defer origin.Close()
	sender := newRecordingTunnelSender()
	manager := NewRelayManager(proxyForTestServers(t, origin), sender)
	defer manager.Reset(context.Canceled)
	manager.Handle(context.Background(), relayOpen("redirect", "deployment-0", "/", true))
	waitRelayFrame(t, sender, "redirect", RelayFrameReset)
	if requests.Load() != 0 {
		t.Fatal("upgrade followed redirect outside deployment binding")
	}
	waitRelayWorkers(t, manager, 0)
}

func TestWebSocketLimitsIncludePendingHandshakes(t *testing.T) {
	entered := make(chan struct{}, 10)
	release := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer origin.Close()
	defer close(release)
	sender := newRecordingTunnelSender()
	manager := NewRelayManager(proxyForTestServers(t, origin, origin), sender)
	manager.limits.webSockets = 3
	manager.limits.webSocketsPerDeployment = 2
	defer manager.Reset(context.Canceled)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		manager.Handle(ctx, relayOpen(fmt.Sprint(i), "deployment-0", "/", true))
	}
	manager.Handle(ctx, relayOpen("per-deployment", "deployment-0", "/", true))
	waitRelayFrame(t, sender, "per-deployment", RelayFrameReset)
	manager.Handle(ctx, relayOpen("third", "deployment-1", "/", true))
	manager.Handle(ctx, relayOpen("global", "deployment-1", "/", true))
	waitRelayFrame(t, sender, "global", RelayFrameReset)
	for i := 0; i < 3; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("admitted handshakes did not start")
		}
	}
	manager.Reset(errors.New("disconnect"))
	waitRelayWorkers(t, manager, 0)
	manager.mu.Lock()
	remaining := len(manager.deploymentWebSockets)
	manager.mu.Unlock()
	if remaining != 0 {
		t.Fatal("deployment quota leaked on disconnect")
	}
	// A new data connection can use the released quota.
	manager.Handle(ctx, relayOpen("replacement", "deployment-0", "/", true))
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("reconnect did not release quota")
	}
	manager.Handle(ctx, relayReset("replacement"))
	waitRelayWorkers(t, manager, 0)
}

func TestWebSocketHandshakeTimeoutReleasesQuota(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer origin.Close()
	sender := newRecordingTunnelSender()
	manager := NewRelayManager(proxyForTestServers(t, origin), sender)
	manager.limits.handshakeTimeout = 50 * time.Millisecond
	defer manager.Reset(context.Canceled)
	manager.Handle(context.Background(), relayOpen("timeout", "deployment-0", "/", true))
	frame := waitRelayFrame(t, sender, "timeout", RelayFrameReset)
	if !strings.Contains(frame.Error, "deadline exceeded") {
		t.Fatalf("reset=%+v", frame)
	}
	waitRelayWorkers(t, manager, 0)
}

func TestWebSocketIdleTimeoutClosesSocketAndReleasesQuota(t *testing.T) {
	closed := make(chan struct{}, 2)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, _, _ = conn.Read(r.Context())
		closed <- struct{}{}
	}))
	defer origin.Close()
	sender := newRecordingTunnelSender()
	manager := NewRelayManager(proxyForTestServers(t, origin), sender)
	manager.limits.webSockets = 1
	manager.limits.webSocketsPerDeployment = 1
	manager.limits.idleTimeout = 100 * time.Millisecond
	defer manager.Reset(context.Canceled)
	manager.Handle(context.Background(), relayOpen("idle", "deployment-0", "/", true))
	waitRelayFrame(t, sender, "idle", RelayFrameHeaders)
	if frame := waitRelayFrame(t, sender, "idle", RelayFrameReset); frame.Error != errRelayIdle.Error() {
		t.Fatalf("reset=%+v", frame)
	}
	waitRelayWorkers(t, manager, 0)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("idle upstream socket not closed")
	}
	manager.Handle(context.Background(), relayOpen("next", "deployment-0", "/", true))
	waitRelayFrame(t, sender, "next", RelayFrameHeaders)
	manager.Handle(context.Background(), relayReset("next"))
	waitRelayWorkers(t, manager, 0)
}

func TestOversizedFrameCancelsOnlyItsStream(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, "healthy")
	}))
	defer origin.Close()
	sender := newRecordingTunnelSender()
	manager := NewRelayManager(proxyForTestServers(t, origin), sender)
	defer manager.Reset(context.Canceled)
	manager.Handle(context.Background(), relayOpen("oversized", "deployment-0", "/slow", false))
	manager.Handle(context.Background(), RelayFrame{Protocol: RelayProtocol, Type: RelayFrameData, Kind: RelayKindHTTP, StreamID: "oversized", Data: make([]byte, maxRelayBufferedBytes+1)})
	waitRelayFrame(t, sender, "oversized", RelayFrameReset)
	manager.Handle(context.Background(), relayOpen("healthy", "deployment-0", "/ok", false))
	waitRelayFrame(t, sender, "healthy", RelayFrameEnd)
	waitRelayWorkers(t, manager, 0)
}

type relayRoundTripper func(*http.Request) (*http.Response, error)

func (f relayRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBlockedWebSocketWriteHasDeadlineWithoutBlockingReceiver(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "healthy") }))
	defer origin.Close()
	sender := newRecordingTunnelSender()
	proxy := proxyForTestServers(t, origin)
	upstream, peer := net.Pipe()
	defer peer.Close()
	// The upgraded peer never reads: net.Pipe makes write backpressure
	// deterministic without relying on the operating system's socket buffers.
	proxy.webSocketClient.Transport = relayRoundTripper(func(r *http.Request) (*http.Response, error) {
		key := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		return &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{
			"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-Websocket-Accept": {base64.StdEncoding.EncodeToString(key[:])},
		}, Body: upstream}, nil
	})
	manager := NewRelayManager(proxy, sender)
	manager.limits.writeTimeout = 50 * time.Millisecond
	defer manager.Reset(context.Canceled)
	manager.Handle(context.Background(), relayOpen("blocked", "deployment-0", "/ws", true))
	waitRelayFrame(t, sender, "blocked", RelayFrameHeaders)
	handled := make(chan struct{})
	go func() {
		manager.Handle(context.Background(), RelayFrame{Protocol: RelayProtocol, Type: RelayFrameData, Kind: RelayKindHTTP, StreamID: "blocked", Data: []byte("payload")})
		manager.Handle(context.Background(), relayOpen("healthy", "deployment-0", "/ok", false))
		close(handled)
	}()
	select {
	case <-handled:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("socket write blocked receiver")
	}
	waitRelayFrame(t, sender, "healthy", RelayFrameEnd)
	waitRelayFrame(t, sender, "blocked", "")
	waitRelayWorkers(t, manager, 0)
}

func TestResetCancelsStreamRetiredBeforeTerminalWrite(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer origin.Close()
	writing := make(chan struct{})
	stopped := make(chan struct{})
	sender := relayFrameSenderFunc(func(ctx context.Context, frame RelayFrame) error {
		if frame.Type == RelayFrameEnd {
			close(writing)
			<-ctx.Done()
			close(stopped)
			return ctx.Err()
		}
		return nil
	})
	manager := NewRelayManager(proxyForTestServers(t, origin), sender)
	defer manager.Reset(context.Canceled)
	manager.Handle(context.Background(), relayOpen("retired", "deployment-0", "/", false))
	select {
	case <-writing:
	case <-time.After(time.Second):
		t.Fatal("terminal frame not attempted")
	}
	manager.mu.Lock()
	retired := manager.streams["retired"] == nil
	manager.mu.Unlock()
	if !retired {
		t.Fatal("stream ID still registered during terminal write")
	}
	manager.Reset(errors.New("disconnect"))
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("retired producer was not canceled on disconnect")
	}
	waitRelayWorkers(t, manager, 0)
}

func TestHTTPIdleTimeoutReleasesUpstreamCapacity(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, "healthy")
	}))
	defer origin.Close()
	sender := newRecordingTunnelSender()
	proxy := proxyForTestServers(t, origin)
	proxy.requests = make(chan struct{}, 1)
	manager := NewRelayManager(proxy, sender)
	manager.limits.idleTimeout = 100 * time.Millisecond
	defer manager.Reset(context.Canceled)
	manager.Handle(context.Background(), relayOpen("idle", "deployment-0", "/slow", false))
	waitRelayFrame(t, sender, "idle", RelayFrameHeaders)
	if frame := waitRelayFrame(t, sender, "idle", RelayFrameReset); frame.Error != errRelayIdle.Error() {
		t.Fatalf("reset=%+v", frame)
	}
	waitRelayWorkers(t, manager, 0)
	manager.Handle(context.Background(), relayOpen("next", "deployment-0", "/ok", false))
	waitRelayFrame(t, sender, "next", RelayFrameEnd)
	waitRelayWorkers(t, manager, 0)
}
