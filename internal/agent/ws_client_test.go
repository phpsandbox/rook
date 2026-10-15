package agent

import (
	"context"
	"errors"
	"io"
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
	client := NewWSClient("ws"+strings.TrimPrefix(server.URL, "http"), "test", true)
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

func TestCanceledRelaySendDoesNotCloseReplacementConnection(t *testing.T) {
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(r.Context()); err == nil {
			received <- struct{}{}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client := NewWSClient("ws"+strings.TrimPrefix(server.URL, "http"), "test", true)
	if err := client.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	oldCtx, stopOld := context.WithCancel(ctx)
	stopOld()
	frame := RelayFrame{Protocol: RelayProtocol, Type: RelayFrameEnd, StreamID: "old", Kind: RelayKindHTTP}
	for i := 0; i < 100; i++ {
		if err := client.SendRelayFrame(oldCtx, frame); !errors.Is(err, context.Canceled) {
			t.Fatalf("stale send: %v", err)
		}
	}
	frame.StreamID = "new"
	if err := client.SendRelayFrame(ctx, frame); err != nil {
		t.Fatalf("replacement connection was closed: %v", err)
	}
	select {
	case <-received:
	case <-ctx.Done():
		t.Fatal("replacement connection did not receive frame")
	}
}

func TestCancelDuringRelayWriteDoesNotCloseSharedConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	var client *WSClient
	headerRead := make(chan struct{})
	release := make(chan struct{})
	received := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			received <- err
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(24 << 20)
		_, reader, err := conn.Reader(ctx)
		if err != nil {
			received <- err
			return
		}
		close(headerRead)
		select {
		case <-release:
		case <-ctx.Done():
			return
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			received <- err
			return
		}
		_, _, err = conn.Read(ctx)
		received <- err
	}))
	t.Cleanup(func() {
		cancel()
		if client != nil {
			client.Close()
		}
		server.Close()
	})
	client = NewWSClient("ws"+strings.TrimPrefix(server.URL, "http"), "test", true)
	if err := client.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	written := make(chan error, 1)
	go func() {
		written <- client.SendRelayFrame(streamCtx, RelayFrame{Protocol: RelayProtocol, Type: RelayFrameData, StreamID: "canceled", Kind: RelayKindHTTP, Data: make([]byte, 20<<20)})
	}()
	select {
	case <-headerRead:
	case <-ctx.Done():
		t.Fatal("relay write did not start")
	}
	cancelStream()
	select {
	case err := <-written:
		t.Fatalf("canceled stream interrupted an in-flight frame: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	// A producer waiting behind that frame must honor its own cancellation.
	queuedCtx, cancelQueued := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancelQueued()
	if err := client.SendRelayFrame(queuedCtx, RelayFrame{Protocol: RelayProtocol, Type: RelayFrameEnd, StreamID: "queued", Kind: RelayKindHTTP}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued write: %v", err)
	}
	close(release)
	if err := <-written; err != nil {
		t.Fatalf("in-flight frame failed: %v", err)
	}
	if err := client.SendRelayFrame(ctx, RelayFrame{Protocol: RelayProtocol, Type: RelayFrameEnd, StreamID: "healthy", Kind: RelayKindHTTP}); err != nil {
		t.Fatalf("healthy stream lost shared connection: %v", err)
	}
	select {
	case err := <-received:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("healthy frame did not arrive")
	}
}

func TestControlConnectionRejectsCleartextBeforeSendingCredential(t *testing.T) {
	hit := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit <- struct{}{} }))
	defer server.Close()
	client := NewWSClient(server.URL, "secret", false)
	if err := client.Connect(context.Background()); err == nil {
		t.Fatal("cleartext control connection accepted")
	}
	select {
	case <-hit:
		t.Fatal("credential sent over cleartext")
	default:
	}
}

func TestControlConnectionRejectsRedirectAndTLSDowngrade(t *testing.T) {
	hit := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit <- struct{}{} }))
	defer target.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing bearer credential")
		}
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()
	client := NewWSClient(origin.URL, "secret", false)
	client.httpClient.Transport = origin.Client().Transport
	if err := client.Connect(context.Background()); err == nil {
		t.Fatal("redirect accepted")
	}
	select {
	case <-hit:
		t.Fatal("control redirect reached another endpoint")
	default:
	}
}

func TestControlConnectionStillVerifiesTLSWithDevelopmentOverride(t *testing.T) {
	hit := make(chan struct{}, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit <- struct{}{} }))
	defer server.Close()
	client := NewWSClient(server.URL, "secret", true)
	if err := client.Connect(context.Background()); err == nil {
		t.Fatal("untrusted TLS certificate accepted")
	}
	select {
	case <-hit:
		t.Fatal("credential reached an untrusted TLS endpoint")
	default:
	}
}
