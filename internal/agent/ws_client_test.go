package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestBackpressuredWriteDoesNotHoldConnectionStateLock(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		<-release
	}))
	defer server.Close()
	defer close(release)
	client := NewWSClient("ws"+strings.TrimPrefix(server.URL, "http"), "test")
	if err := client.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	writeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	writing := make(chan error, 1)
	go func() {
		writing <- client.Send(writeCtx, OutboundMessage{Type: "log", Content: strings.Repeat("x", 8<<20)})
	}()
	time.Sleep(100 * time.Millisecond)
	readCtx, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	started := time.Now()
	_, _, _ = client.ReadRaw(readCtx)
	if time.Since(started) > 300*time.Millisecond {
		t.Fatal("backpressured write held connection lock past read deadline")
	}
	<-writing
}
