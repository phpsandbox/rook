package agent

import (
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
