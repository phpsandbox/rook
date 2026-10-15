package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/phpsandbox/rook/internal/host"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyOpenHTTPPreservesHeaderPairsAndBinaryBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Host != "app.example.test" {
			t.Fatalf("host = %q", r.Host)
		}
		if got := r.Header.Values("X-Multi"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
			t.Fatalf("X-Multi = %#v", got)
		}
		if strings.Contains(r.Header.Get("Accept-Encoding"), "br") || strings.Contains(r.Header.Get("Accept-Encoding"), "zstd") {
			t.Fatalf("client accept-encoding leaked to origin: %q", r.Header.Get("Accept-Encoding"))
		}
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

	resp, err := NewProxy(state).OpenHTTP(
		context.Background(),
		"deployment-1",
		http.MethodPost,
		"upload",
		[]HeaderPair{
			{"host", "app.example.test"},
			{"X-Multi", "one"},
			{"X-Multi", "two"},
			{"Accept-Encoding", "zstd, br"},
		},
		bytes.NewReader([]byte{0, 1, 2, 255}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Values("Set-Cookie"); len(got) != 2 || got[0] != "a=1; Path=/" || got[1] != "b=2; Path=/" {
		t.Fatalf("Set-Cookie = %#v", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, []byte{5, 6, 7, 255}) {
		t.Fatalf("response body = %#v", body)
	}
}

func TestProxyReusesConnectionsAcrossConcurrentWaves(t *testing.T) {
	const count = 16
	entered := make(chan struct{}, count)
	release := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wave, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		entered <- struct{}{}
		select {
		case <-release[wave]:
			_, _ = io.WriteString(w, "ok")
		case <-r.Context().Done():
		}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proxy := proxyForTestServers(t, server)
	for wave := range release {
		results := make(chan error, count)
		for i := 0; i < count; i++ {
			go func() {
				resp, err := proxy.OpenHTTP(ctx, "deployment-0", http.MethodGet, fmt.Sprintf("/%d", wave), nil, nil)
				if err == nil {
					body, readErr := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					err = readErr
					if err == nil && string(body) != "ok" {
						err = fmt.Errorf("body = %q", body)
					}
				}
				results <- err
			}()
		}
		for i := 0; i < count; i++ {
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("concurrent requests did not reach origin")
			}
		}
		close(release[wave])
		for i := 0; i < count; i++ {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := connections.Load(); got != count {
		t.Fatalf("two waves opened %d TCP connections, want %d", got, count)
	}
}

func TestProxyLimitsAcrossDeploymentsUntilResponsesClose(t *testing.T) {
	entered := make(chan struct{}, 8)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		_, _ = io.WriteString(w, "ok")
	})
	first := httptest.NewServer(handler)
	defer first.Close()
	second := httptest.NewServer(handler)
	defer second.Close()
	proxy := proxyForTestServers(t, first, second)
	proxy.requests = make(chan struct{}, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var held []*http.Response
	for i := 0; i < 2; i++ {
		resp, err := proxy.OpenHTTP(ctx, fmt.Sprintf("deployment-%d", i), http.MethodGet, "/", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatal(err)
		}
		held = append(held, resp)
		<-entered
	}
	waitCtx, stopWaiting := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() {
		_, err := proxy.OpenHTTP(waitCtx, "deployment-0", http.MethodGet, "/", nil, nil)
		result <- err
	}()
	select {
	case <-entered:
		t.Fatal("third request bypassed the global response limit")
	case err := <-result:
		t.Fatalf("queued request returned early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	stopWaiting()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued request: %v", err)
	}
	if _, err := proxy.OpenHTTP(ctx, "deployment-0", http.MethodPost, "/", nil, strings.NewReader("upload")); !errors.Is(err, errHTTPRelayOverloaded) {
		t.Fatalf("saturated upload: %v", err)
	}
	if err := held[0].Body.Close(); err != nil {
		t.Fatal(err)
	}
	// Closing twice must not consume another request's slot or block.
	doubleClosed := make(chan error, 1)
	go func() { doubleClosed <- held[0].Body.Close() }()
	select {
	case err := <-doubleClosed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("double close blocked")
	}
	resp, err := proxy.OpenHTTP(ctx, "deployment-0", http.MethodGet, "/", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-entered
}

func TestProxyFailedRequestReleasesCapacity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer server.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	proxy := proxyForTestServers(t, server, closed)
	closed.Close()
	proxy.requests = make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if resp, err := proxy.OpenHTTP(ctx, "deployment-1", http.MethodGet, "/", nil, nil); err == nil {
		resp.Body.Close()
		t.Fatal("closed origin unexpectedly succeeded")
	}
	resp, err := proxy.OpenHTTP(ctx, "deployment-0", http.MethodGet, "/", nil, nil)
	if err != nil {
		t.Fatalf("failed request leaked capacity: %v", err)
	}
	resp.Body.Close()
}

func TestProxyRejectsBodylessRequestsBeyondQueueLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer server.Close()
	proxy := proxyForTestServers(t, server)
	proxy.requests = make(chan struct{}, 1)
	proxy.waiting = make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	held, err := proxy.OpenHTTP(ctx, "deployment-0", http.MethodGet, "/", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Body.Close()
	waitCtx, stopWaiting := context.WithCancel(ctx)
	defer stopWaiting()
	queued := make(chan error, 1)
	go func() {
		_, err := proxy.OpenHTTP(waitCtx, "deployment-0", http.MethodGet, "/", nil, nil)
		queued <- err
	}()
	for len(proxy.waiting) == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("request did not enter queue")
		case <-time.After(time.Millisecond):
		}
	}
	overCtx, stopOver := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stopOver()
	if _, err := proxy.OpenHTTP(overCtx, "deployment-0", http.MethodGet, "/", nil, nil); !errors.Is(err, errHTTPRelayOverloaded) {
		t.Fatalf("full queue: %v", err)
	}
	stopWaiting()
	if err := <-queued; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancellation: %v", err)
	}
	held.Body.Close()
	resp, err := proxy.OpenHTTP(ctx, "deployment-0", http.MethodGet, "/", nil, nil)
	if err != nil {
		t.Fatalf("capacity was not reclaimed: %v", err)
	}
	resp.Body.Close()
}

func proxyForTestServers(t *testing.T, servers ...*httptest.Server) *Proxy {
	t.Helper()
	state := host.NewBindings(t.TempDir())
	for i, server := range servers {
		_, portString, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		port, err := strconv.Atoi(portString)
		if err != nil {
			t.Fatal(err)
		}
		if err := state.Set(fmt.Sprintf("deployment-%d", i), host.Binding{Port: port}); err != nil {
			t.Fatal(err)
		}
	}
	return NewProxy(state)
}

func TestProxyOpenHTTPReturnsRedirectToBrowser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/weather" {
			t.Fatalf("proxy followed redirect to %s", r.URL.Path)
		}
		w.Header().Add("Set-Cookie", "session=updated; Path=/; HttpOnly")
		http.Redirect(w, r, "/result", http.StatusSeeOther)
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
	resp, err := NewProxy(state).OpenHTTP(context.Background(), "deployment-1", http.MethodPost, "/weather", nil, strings.NewReader("city=Berlin"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/result" {
		t.Fatalf("redirect = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp.Header.Get("Set-Cookie") != "session=updated; Path=/; HttpOnly" {
		t.Fatal("redirect session cookie was lost")
	}
}
