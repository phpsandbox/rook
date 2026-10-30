package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const runtimeContainerID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func runtimeBinding(t *testing.T, bindings *Bindings) Binding {
	t.Helper()
	binding := Binding{Port: 32788, Metadata: json.RawMessage(`{"containerId":"` + runtimeContainerID + `","releaseId":"release-a","port":32788,"healthPath":"/health","future":{"keep":true}}`)}
	if err := bindings.Set("app", binding); err != nil {
		t.Fatal(err)
	}
	return binding
}

func runtimeResolver(t *testing.T, bindings *Bindings, handler http.HandlerFunc) *RuntimeBindings {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	resolver := NewRuntimeBindings(bindings)
	t.Cleanup(resolver.Close)
	// Keep the production HTTP client (including its timeout and redirect policy)
	// while directing its Docker connection to the test daemon.
	resolver.client.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}
	return resolver
}

func runtimeInspection(port int) string {
	return fmt.Sprintf(`{"Id":%q,"State":{"Running":true},"NetworkSettings":{"Ports":{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":%q}]}}}`, runtimeContainerID, fmt.Sprint(port))
}

func TestRuntimeBindingTracksPortChangesWithoutAgentRestart(t *testing.T) {
	bindings := NewBindings(t.TempDir())
	runtimeBinding(t, bindings)
	ports := make(chan int, 3)
	ports <- 32770
	ports <- 32771
	ports <- 32771
	resolver := runtimeResolver(t, bindings, func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/containers/"+runtimeContainerID+"/json" {
			t.Errorf("unexpected Docker request: %s %s", request.Method, request.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case port := <-ports:
			_, _ = io.WriteString(w, runtimeInspection(port))
		default:
			t.Error("unexpected extra Docker inspection")
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	first, err := resolver.Resolve(context.Background(), "app")
	if err != nil || first.Port != 32770 {
		t.Fatalf("first reconciliation: %+v, %v", first, err)
	}

	second, err := resolver.Resolve(context.Background(), "app")
	if err != nil || second.Port != 32771 {
		t.Fatalf("second reconciliation: %+v, %v", second, err)
	}

	reloaded := NewBindings(bindings.dir)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	persisted, ok := reloaded.Get("app")
	if !ok || persisted.Port != 32771 {
		t.Fatalf("persisted binding: %+v, exists=%v", persisted, ok)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(persisted.Metadata, &fields); err != nil {
		t.Fatal(err)
	}
	var future struct {
		Keep bool
	}
	if err := json.Unmarshal(fields["future"], &future); err != nil {
		t.Fatal(err)
	}
	if string(fields["port"]) != "32771" || !future.Keep || string(fields["releaseId"]) != `"release-a"` {
		t.Fatalf("reconciliation lost release metadata: %s", persisted.Metadata)
	}

	// An unchanged mapping must not require another disk write.
	bindings.dir = filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(bindings.dir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	unchanged, err := resolver.Resolve(context.Background(), "app")
	if err != nil || unchanged.Port != 32771 {
		t.Fatalf("unchanged reconciliation: %+v, %v", unchanged, err)
	}
}

func TestRuntimeBindingRejectsUnavailableOrAmbiguousContainers(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		error  string
	}{
		"missing":             {http.StatusNotFound, `{}`, "Docker returned HTTP 404"},
		"daemon error":        {http.StatusInternalServerError, `internal diagnostic`, "Docker returned HTTP 500"},
		"malformed":           {http.StatusOK, `{`, "decode active container"},
		"stopped":             {http.StatusOK, strings.Replace(runtimeInspection(32770), `"Running":true`, `"Running":false`, 1), "not running stably"},
		"restarting":          {http.StatusOK, strings.Replace(runtimeInspection(32770), `"Running":true`, `"Running":true,"Restarting":true`, 1), "not running stably"},
		"paused":              {http.StatusOK, strings.Replace(runtimeInspection(32770), `"Running":true`, `"Running":true,"Paused":true`, 1), "not running stably"},
		"different container": {http.StatusOK, strings.ReplaceAll(runtimeInspection(32770), runtimeContainerID, strings.Repeat("b", 64)), "different container identity"},
		"public interface":    {http.StatusOK, strings.ReplaceAll(runtimeInspection(32770), "127.0.0.1", "0.0.0.0"), "no loopback TCP port"},
		"UDP only":            {http.StatusOK, strings.ReplaceAll(runtimeInspection(32770), "8080/tcp", "8080/udp"), "no loopback TCP port"},
		"invalid port":        {http.StatusOK, runtimeInspection(65536), "invalid loopback TCP port"},
		"zero port":           {http.StatusOK, runtimeInspection(0), "invalid loopback TCP port"},
		"multiple ports":      {http.StatusOK, strings.Replace(runtimeInspection(32770), `"Ports":{`, `"Ports":{"9090/tcp":[{"HostIp":"127.0.0.1","HostPort":"32772"}],`, 1), "multiple loopback TCP ports"},
	}
	for name, scenario := range cases {
		t.Run(name, func(t *testing.T) {
			bindings := NewBindings(t.TempDir())
			original := runtimeBinding(t, bindings)
			resolver := runtimeResolver(t, bindings, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(scenario.status)
				_, _ = io.WriteString(w, scenario.body)
			})

			binding, err := resolver.Resolve(context.Background(), "app")
			if err == nil || !strings.Contains(err.Error(), scenario.error) || binding.Port != 0 {
				t.Fatalf("unsafe resolution: %+v, %v; expected %q", binding, err, scenario.error)
			}

			current, ok := bindings.Get("app")
			if !ok || current.Port != original.Port || string(current.Metadata) != string(original.Metadata) {
				t.Fatalf("failed inspection changed binding: %+v", current)
			}
		})
	}
}

func TestRuntimeBindingDoesNotOverwriteConcurrentPublication(t *testing.T) {
	bindings := NewBindings(t.TempDir())
	original := runtimeBinding(t, bindings)
	replacement := Binding{Port: 40000, Metadata: json.RawMessage(`{"containerId":"` + strings.Repeat("b", 64) + `","port":40000}`)}
	resolver := runtimeResolver(t, bindings, func(w http.ResponseWriter, _ *http.Request) {
		if err := bindings.Set("app", replacement); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, runtimeInspection(32770))
	})

	resolved, err := resolver.Resolve(context.Background(), "app")
	if err == nil || !strings.Contains(err.Error(), "binding changed") || resolved.Port != 0 {
		t.Fatalf("superseded binding was used: %+v, %v", resolved, err)
	}

	current, ok := bindings.Get("app")
	if !ok || current.Port != replacement.Port || string(current.Metadata) != string(replacement.Metadata) {
		t.Fatalf("concurrent publication was overwritten: original=%+v, current=%+v", original, current)
	}
}

func TestRuntimeBindingDoesNotResurrectUnpublishedApp(t *testing.T) {
	bindings := NewBindings(t.TempDir())
	runtimeBinding(t, bindings)
	resolver := runtimeResolver(t, bindings, func(w http.ResponseWriter, _ *http.Request) {
		if err := bindings.Remove("app"); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, runtimeInspection(32770))
	})

	resolved, err := resolver.Resolve(context.Background(), "app")
	if err == nil || !strings.Contains(err.Error(), "binding changed") || resolved.Port != 0 {
		t.Fatalf("unpublished binding was used: %+v, %v", resolved, err)
	}
	if _, exists := bindings.Get("app"); exists {
		t.Fatal("unpublished binding was recreated")
	}
}

func TestRuntimeBindingPersistenceFailurePreservesOldBinding(t *testing.T) {
	bindings := NewBindings(t.TempDir())
	original := runtimeBinding(t, bindings)
	resolver := runtimeResolver(t, bindings, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, runtimeInspection(32770))
	})
	bindings.dir = filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(bindings.dir, nil, 0600); err != nil {
		t.Fatal(err)
	}

	resolved, err := resolver.Resolve(context.Background(), "app")
	if err == nil || !strings.Contains(err.Error(), "persist reconciled binding") || resolved.Port != 0 {
		t.Fatalf("unpersisted binding was used: %+v, %v", resolved, err)
	}
	current, ok := bindings.Get("app")
	if !ok || current.Port != original.Port || string(current.Metadata) != string(original.Metadata) {
		t.Fatalf("failed persistence changed binding: %+v", current)
	}
}

func TestRuntimeBindingCancellationDoesNotUseSavedPort(t *testing.T) {
	bindings := NewBindings(t.TempDir())
	runtimeBinding(t, bindings)
	resolver := NewRuntimeBindings(bindings)
	t.Cleanup(resolver.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resolved, err := resolver.Resolve(ctx, "app")
	if !errors.Is(err, context.Canceled) || resolved.Port != 0 {
		t.Fatalf("canceled resolution: %+v, %v", resolved, err)
	}
}

func TestRuntimeBindingConcurrentReads(t *testing.T) {
	bindings := NewBindings(t.TempDir())
	runtimeBinding(t, bindings)
	resolver := runtimeResolver(t, bindings, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, runtimeInspection(32770))
	})
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			resolved, err := resolver.Resolve(context.Background(), "app")
			if err != nil || resolved.Port != 32770 {
				t.Errorf("concurrent resolution: %+v, %v", resolved, err)
			}
		})
	}
	workers.Wait()
}

func TestRuntimeBindingInspectsConfiguredLocalDockerSocket(t *testing.T) {
	directory, err := os.MkdirTemp("", "rook-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var port atomic.Int64
	port.Store(32770)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/containers/"+runtimeContainerID+"/json" {
			t.Errorf("unexpected Docker request: %s %s", request.Method, request.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, runtimeInspection(int(port.Load())))
	})}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		if err := <-finished; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Docker test daemon: %v", err)
		}
	})
	t.Setenv("DOCKER_HOST", "unix://"+socket)
	t.Setenv("DOCKER_CONTEXT", "")
	bindings := NewBindings(t.TempDir())
	runtimeBinding(t, bindings)
	resolver := NewRuntimeBindings(bindings)
	t.Cleanup(resolver.Close)

	first, err := resolver.Resolve(context.Background(), "app")
	if err != nil || first.Port != 32770 {
		t.Fatalf("Unix socket resolution: %+v, %v", first, err)
	}

	port.Store(32771)
	second, err := resolver.Resolve(context.Background(), "app")
	if err != nil || second.Port != 32771 {
		t.Fatalf("Unix socket resolution after port change: %+v, %v", second, err)
	}
}

func TestRuntimeBindingRejectsMissingContainerIdentityBeforeInspecting(t *testing.T) {
	bindings := NewBindings(t.TempDir())
	if err := bindings.Set("app", Binding{Port: 32788}); err != nil {
		t.Fatal(err)
	}
	resolver := runtimeResolver(t, bindings, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("inspected Docker without a valid container identity")
		w.WriteHeader(http.StatusBadRequest)
	})

	resolved, err := resolver.Resolve(context.Background(), "app")
	if err == nil || !strings.Contains(err.Error(), "decode active binding") || resolved.Port != 0 {
		t.Fatalf("resolved unidentified binding: %+v, %v", resolved, err)
	}
}
