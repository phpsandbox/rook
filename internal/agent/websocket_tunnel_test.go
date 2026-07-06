package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestWebSocketTunnelManagerProxiesFrames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		messageType, data, err := conn.Read(r.Context())
		if err != nil {
			t.Errorf("read websocket: %v", err)
			return
		}
		if messageType != websocket.MessageBinary || string(data) != "\x00payload" {
			t.Errorf("message = type %v data %#v", messageType, data)
			return
		}
		if err := conn.Write(r.Context(), websocket.MessageText, []byte("echo")); err != nil {
			t.Errorf("write websocket: %v", err)
		}
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
	manager := NewWebSocketTunnelManager(NewProxy(state), sender)
	ctx := context.Background()

	manager.Handle(ctx, InboundMessage{
		Type:      "websocket.start",
		CommandID: "cmd-1",
		decoded: WebSocketStartPayload{
			RequestID:    "req-1",
			DeploymentID: "deployment-1",
			Path:         "/socket",
			Headers:      []HeaderPair{{"x-original-host", "app.example.test"}},
		},
	})

	messages := sender.waitForCount(t, 1, 2*time.Second)
	if messages[0].Type != "websocket.open" {
		t.Fatalf("open message = %#v", messages[0])
	}

	manager.Handle(ctx, InboundMessage{
		Type:      "websocket.message",
		CommandID: "cmd-1",
		decoded: WebSocketMessagePayload{
			RequestID: "req-1",
			Data:      []byte("\x00payload"),
			Text:      false,
		},
	})

	messages = sender.waitForCount(t, 2, 2*time.Second)
	if messages[1].Type != "websocket.message" || !messages[1].Text || string(messages[1].Data) != "echo" {
		t.Fatalf("echo message = %#v", messages[1])
	}

	manager.Handle(ctx, InboundMessage{
		Type:      "websocket.close",
		CommandID: "cmd-1",
		decoded:   WebSocketClosePayload{RequestID: "req-1", Code: int(websocket.StatusNormalClosure)},
	})
}
