package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phpsandbox/rook/internal/agent"
)

func TestAgentPlaneURLUsesWebSocketScheme(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		controlPlane string
		expected     string
	}{
		"HTTP":  {controlPlane: "http://rook.phpsandbox.test/connect", expected: "ws://rook.phpsandbox.test/connect?channel=control&server_id=server-1"},
		"HTTPS": {controlPlane: "https://rook.phpsandbox.io/connect", expected: "wss://rook.phpsandbox.io/connect?channel=control&server_id=server-1"},
		"WS":    {controlPlane: "ws://rook.phpsandbox.test/connect", expected: "ws://rook.phpsandbox.test/connect?channel=control&server_id=server-1"},
		"WSS":   {controlPlane: "wss://rook.phpsandbox.io/connect", expected: "wss://rook.phpsandbox.io/connect?channel=control&server_id=server-1"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			actual := agentPlaneURL(test.controlPlane, "server-1", "control")
			if actual != test.expected {
				t.Fatalf("agentPlaneURL() = %q, want %q", actual, test.expected)
			}
		})
	}
}

func TestCorruptOperationStateNeverConnects(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) }))
	defer server.Close()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "operations"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "operations", "invalid.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := run(ctx, agent.Config{ServerID: "test", Token: "test", ControlPlane: server.URL, StateDir: dir})
	if err == nil || !strings.Contains(err.Error(), "recover operations") || requests.Load() != 0 {
		t.Fatalf("corrupt host connected: %v, requests=%d", err, requests.Load())
	}
}
