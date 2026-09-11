package agent

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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

	state := NewStateStore(t.TempDir())
	if err := state.Set("deployment-1", DeploymentState{ContainerID: "container-1", Port: port}); err != nil {
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

	state := NewStateStore(t.TempDir())
	if err := state.Set("deployment-1", DeploymentState{ContainerID: "container-1", Port: port}); err != nil {
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
