package agent

import (
	"bytes"
	"context"
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
	manager := NewRelayManager(NewProxy(state), sender)
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
	manager := NewRelayManager(NewProxy(state), sender)
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
