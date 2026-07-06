package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestProxyHandleHTTPUsesDeploymentStateAndStringBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Proxy-Test", r.Header.Get("X-Original-Host"))
		_, _ = fmt.Fprintf(w, "%s %s %s", r.Method, r.URL.Path, string(body))
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

	status, headers, body, err := NewProxy(state).HandleHTTP(
		"deployment-1",
		http.MethodPost,
		"/submit",
		map[string]string{"X-Original-Host": "app.example.test"},
		"payload-body",
	)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if headers["X-Proxy-Test"] != "app.example.test" {
		t.Fatalf("header = %q", headers["X-Proxy-Test"])
	}
	if body != "POST /submit payload-body" {
		t.Fatalf("body = %q", body)
	}
}

func TestProxyOpenHTTPPreservesHeaderPairsAndBinaryBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Host != "app.example.test" {
			t.Fatalf("host = %q", r.Host)
		}
		if got := r.Header.Values("X-Multi"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
			t.Fatalf("X-Multi = %#v", got)
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

	state := NewStateStore(t.TempDir())
	if err := state.Set("deployment-1", DeploymentState{ContainerID: "container-1", Port: port}); err != nil {
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
