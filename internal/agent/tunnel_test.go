package agent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/phpsandbox/rook/internal/host"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"nhooyr.io/websocket"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRelayManagerStreamsHTTPRequestAndResponseBytes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, []byte{0, 1, 2, 255}) {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Add("Set-Cookie", "a=1; Path=/")
		w.Header().Add("Set-Cookie", "b=2; Path=/")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte{5, 6, 7, 255})
	}))
	defer server.Close()

	_, portString, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatal(err)
	}

	state := host.NewBindings(t.TempDir())
	if err := state.Set("deployment-1", host.Binding{Port: port}); err != nil {
		t.Fatal(err)
	}

	sender := newRecordingTunnelSender()
	manager := NewRelayManager(NewProxy(savedTestBindings{state}), sender)
	ctx := context.Background()

	manager.Handle(ctx, RelayFrame{
		Protocol:     RelayProtocol,
		Type:         RelayFrameOpen,
		StreamID:     "req-1",
		Kind:         RelayKindHTTP,
		DeploymentID: "deployment-1",
		Method:       http.MethodPost,
		Path:         "/upload",
		Headers:      []HeaderPair{{"host", "app.example.test"}},
		HasBody:      boolPtr(true),
	})
	manager.Handle(ctx, RelayFrame{
		Protocol: RelayProtocol,
		Type:     RelayFrameData,
		StreamID: "req-1",
		Kind:     RelayKindHTTP,
		Data:     []byte{0, 1},
	})
	manager.Handle(ctx, RelayFrame{
		Protocol: RelayProtocol,
		Type:     RelayFrameData,
		StreamID: "req-1",
		Kind:     RelayKindHTTP,
		Data:     []byte{2, 255},
	})
	manager.Handle(ctx, RelayFrame{
		Protocol: RelayProtocol,
		Type:     RelayFrameEnd,
		StreamID: "req-1",
		Kind:     RelayKindHTTP,
	})

	messages := sender.wait(t, 2*time.Second)
	if len(messages) != 3 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0].Type != RelayFrameHeaders || messages[0].Status != http.StatusCreated {
		t.Fatalf("start message = %#v", messages[0])
	}
	if got := headerValues(messages[0].Headers, "Set-Cookie"); len(got) != 2 || got[0] != "a=1; Path=/" || got[1] != "b=2; Path=/" {
		t.Fatalf("Set-Cookie = %#v", got)
	}
	if messages[1].Type != RelayFrameData || !bytes.Equal(messages[1].Data, []byte{5, 6, 7, 255}) {
		t.Fatalf("body message = %#v", messages[1])
	}
	if messages[2].Type != RelayFrameEnd {
		t.Fatalf("end message = %#v", messages[2])
	}
}

func TestRelayManagerProxiesGETWithoutRequestBodyStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != http.NoBody {
			body, _ := io.ReadAll(r.Body)
			if len(body) != 0 {
				t.Fatalf("body = %#v", body)
			}
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	_, portString, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatal(err)
	}

	state := host.NewBindings(t.TempDir())
	if err := state.Set("deployment-1", host.Binding{Port: port}); err != nil {
		t.Fatal(err)
	}

	sender := newRecordingTunnelSender()
	manager := NewRelayManager(NewProxy(savedTestBindings{state}), sender)
	manager.Handle(context.Background(), RelayFrame{
		Protocol:     RelayProtocol,
		Type:         RelayFrameOpen,
		StreamID:     "req-1",
		Kind:         RelayKindHTTP,
		DeploymentID: "deployment-1",
		Method:       http.MethodGet,
		Path:         "/",
		HasBody:      boolPtr(false),
	})

	messages := sender.wait(t, 2*time.Second)
	if len(messages) != 3 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0].Type != RelayFrameHeaders || messages[0].Status != http.StatusOK {
		t.Fatalf("start message = %#v", messages[0])
	}
	if messages[1].Type != RelayFrameData || !bytes.Equal(messages[1].Data, []byte("ok")) {
		t.Fatalf("body message = %#v", messages[1])
	}
	if messages[2].Type != RelayFrameEnd {
		t.Fatalf("end message = %#v", messages[2])
	}
}

func TestRelayOverloadedUploadUnblocksReceiveLoop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(w, r.Body) }))
	defer server.Close()
	proxy := proxyForTestServers(t, server)
	proxy.requests = make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	held, err := proxy.OpenHTTP(ctx, "deployment-0", http.MethodGet, "/", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Body.Close()
	sender := newRecordingTunnelSender()
	manager := NewRelayManager(proxy, sender)
	manager.Handle(ctx, RelayFrame{Protocol: RelayProtocol, Type: RelayFrameOpen, StreamID: "overloaded", Kind: RelayKindHTTP, DeploymentID: "deployment-0", Method: http.MethodPost, Path: "/", HasBody: boolPtr(true)})
	dataHandled := make(chan struct{})
	go func() {
		manager.Handle(ctx, RelayFrame{Protocol: RelayProtocol, Type: RelayFrameData, StreamID: "overloaded", Kind: RelayKindHTTP, Data: []byte("upload")})
		close(dataHandled)
	}()
	select {
	case <-dataHandled:
	case <-ctx.Done():
		t.Fatal("rejected upload blocked the shared receive loop")
	}
	frames := sender.wait(t, time.Second)
	if len(frames) != 1 || frames[0].Type != RelayFrameReset || frames[0].Error != errHTTPRelayOverloaded.Error() {
		t.Fatalf("overload response: %#v", frames)
	}
	held.Body.Close()
	resp, err := proxy.OpenHTTP(ctx, "deployment-0", http.MethodPost, "/", nil, strings.NewReader("accepted"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "accepted" {
		t.Fatalf("next upload failed: %q, %v", body, err)
	}
}

func TestRelayResetCancelsQueuedBodylessRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	proxy := proxyForTestServers(t, server)
	proxy.requests = make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	held, err := proxy.OpenHTTP(ctx, "deployment-0", http.MethodGet, "/held", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Body.Close()
	manager := NewRelayManager(proxy, newRecordingTunnelSender())
	manager.Handle(ctx, RelayFrame{Protocol: RelayProtocol, Type: RelayFrameOpen, StreamID: "queued", Kind: RelayKindHTTP, DeploymentID: "deployment-0", Method: http.MethodGet, Path: "/canceled"})
	manager.Handle(ctx, RelayFrame{Protocol: RelayProtocol, Type: RelayFrameReset, StreamID: "queued", Kind: RelayKindHTTP, Error: "caller disconnected"})
	held.Body.Close()
	resp, err := proxy.OpenHTTP(ctx, "deployment-0", http.MethodGet, "/healthy", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := requests.Load(); got != 2 {
		t.Fatalf("canceled queued request reached upstream: %d requests", got)
	}
}

func TestRelayReusesCompletedHTTPIDForWebSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/http" {
			_, _ = io.WriteString(w, "ok")
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			received <- err.Error()
			return
		}
		defer conn.CloseNow()
		_, data, err := conn.Read(ctx)
		if err != nil {
			received <- err.Error()
			return
		}
		received <- string(data)
	}))
	defer server.Close()
	defer cancel()
	terminal := make(chan struct{})
	release := make(chan struct{})
	var ends atomic.Int32
	sender := relayFrameSenderFunc(func(ctx context.Context, frame RelayFrame) error {
		if frame.Type == RelayFrameEnd && ends.Add(1) == 1 {
			close(terminal)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	manager := NewRelayManager(proxyForTestServers(t, server), sender)
	defer func() { cancel(); manager.Reset(context.Canceled) }()
	frame := RelayFrame{Protocol: RelayProtocol, Type: RelayFrameOpen, Kind: RelayKindHTTP, StreamID: "reused", DeploymentID: "deployment-0", Method: http.MethodGet, Path: "/http"}
	manager.Handle(ctx, frame)
	select {
	case <-terminal:
	case <-ctx.Done():
		t.Fatal("HTTP request did not end")
	}
	// Simulate a peer reusing the ID as soon as it receives the terminal frame,
	// before the old request goroutine has finished returning.
	frame.Path = "/ws"
	frame.Headers = []HeaderPair{{"Upgrade", "websocket"}}
	manager.Handle(ctx, frame)
	manager.Handle(ctx, RelayFrame{Protocol: RelayProtocol, Type: RelayFrameData, Kind: RelayKindHTTP, StreamID: "reused", Data: []byte("payload"), Text: true})
	select {
	case data := <-received:
		if data != "payload" {
			t.Fatalf("websocket received %q", data)
		}
	case <-ctx.Done():
		t.Fatal("retiring HTTP stream swallowed WebSocket data")
	}
	close(release)
}

type relayFrameSenderFunc func(context.Context, RelayFrame) error

func (f relayFrameSenderFunc) SendRelayFrame(ctx context.Context, frame RelayFrame) error {
	return f(ctx, frame)
}

type recordingTunnelSender struct {
	mu       sync.Mutex
	messages []RelayFrame
	done     chan struct{}
}

func newRecordingTunnelSender() *recordingTunnelSender {
	return &recordingTunnelSender{done: make(chan struct{})}
}

func (s *recordingTunnelSender) SendRelayFrame(ctx context.Context, tunnelMessage RelayFrame) error {
	s.mu.Lock()
	s.messages = append(s.messages, tunnelMessage)
	if tunnelMessage.Type == RelayFrameEnd || tunnelMessage.Type == RelayFrameReset {
		select {
		case <-s.done:
		default:
			close(s.done)
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *recordingTunnelSender) wait(t *testing.T, timeout time.Duration) []RelayFrame {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for tunnel response")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RelayFrame(nil), s.messages...)
}

func (s *recordingTunnelSender) waitForCount(t *testing.T, count int, timeout time.Duration) []RelayFrame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		messages := append([]RelayFrame(nil), s.messages...)
		s.mu.Unlock()
		if len(messages) >= count {
			return messages
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d tunnel messages", count)
	return nil
}

func headerValues(headers []HeaderPair, name string) []string {
	var values []string
	for _, header := range headers {
		if strings.EqualFold(header[0], name) {
			values = append(values, header[1])
		}
	}
	return values
}

func boolPtr(value bool) *bool {
	return &value
}

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
