package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var regexpUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

const currentContainer = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const previousContainer = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const workerContainer = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

type releaseFixture struct {
	store           *ReleaseStore
	options         RestoreReleaseOptions
	calls           [][]string
	active          string
	failImage       bool
	unhealthy       error
	runningWorker   bool
	cleanupCanceled bool
}

func newReleaseFixture(t *testing.T) *releaseFixture {
	t.Helper()
	directory := t.TempDir()
	store, err := NewReleaseStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	f := &releaseFixture{store: store, active: "inactive", options: RestoreReleaseOptions{Deployment: "application", Release: "previous", ExpectCurrent: "current", Service: "rook-test.service"}}
	current := json.RawMessage(fmt.Sprintf(`{"releaseId":"current","containerId":%q,"roleContainers":[%q],"port":3000}`, currentContainer, currentContainer))
	bindings := NewBindings(directory)
	if err := bindings.Set("application", Binding{Port: 3000, Metadata: current}); err != nil {
		t.Fatal(err)
	}
	if err := bindings.Set("unrelated", Binding{Port: 4000, Metadata: json.RawMessage(`{"kept":true}`)}); err != nil {
		t.Fatal(err)
	}
	target := fmt.Sprintf(`{"releaseId":"previous","containerId":%q,"roleContainers":[%q,%q],"port":3001,"healthPath":"/_health","futureField":{"keep":"opaque"}}`, previousContainer, previousContainer, workerContainer)
	if err := writeFile(filepath.Join(directory, "releases", "application", "previous.json"), []byte(target), 0600); err != nil {
		t.Fatal(err)
	}
	store.command = f.command
	store.healthy = func(ctx context.Context, port int, path string) error {
		if port != 3002 || path != "/_health" {
			t.Fatalf("readiness port=%d path=%s", port, path)
		}
		return f.unhealthy
	}
	return f
}

func (f *releaseFixture) command(ctx context.Context, name string, args ...string) (string, error) {
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	key := strings.Join(call, " ")
	switch key {
	case "systemctl show rook-test.service --property=LoadState --value":
		return "loaded", nil
	case "systemctl show rook-test.service --property=ActiveState --value":
		return f.active, nil
	case "docker image inspect --format {{.Id}} sha256:image":
		if f.failImage {
			return "", errors.New("image not found")
		}
		return "sha256:image", nil
	case "docker inspect --format {{.Image}} " + previousContainer, "docker inspect --format {{.Image}} " + workerContainer:
		return "sha256:image", nil
	case "docker inspect --format {{.State.Running}} " + previousContainer:
		return "false", nil
	case "docker inspect --format {{.State.Running}} " + workerContainer:
		return strconv.FormatBool(f.runningWorker), nil
	case "docker start " + previousContainer + " " + workerContainer:
		return "started", nil
	case "docker inspect --format {{json .NetworkSettings.Ports}} " + previousContainer:
		return `{"3333/tcp":[{"HostIp":"127.0.0.1","HostPort":"3002"}]}`, nil
	case "docker inspect --format {{.State.Running}} {{.State.Restarting}} " + previousContainer, "docker inspect --format {{.State.Running}} {{.State.Restarting}} " + workerContainer:
		return "true false", nil
	case "docker stop --time 60 " + currentContainer:
		return "stopped", nil
	case "docker stop --time 30 " + previousContainer, "docker stop --time 30 " + previousContainer + " " + workerContainer:
		f.cleanupCanceled = ctx.Err() != nil
		return "stopped", nil
	default:
		return "", fmt.Errorf("unexpected command: %s", key)
	}
}

func (f *releaseFixture) binding(t *testing.T) Binding {
	t.Helper()
	bindings := NewBindings(f.store.directory)
	if err := bindings.Load(); err != nil {
		t.Fatal(err)
	}
	binding, ok := bindings.Get("application")
	if !ok {
		t.Fatal("binding missing")
	}
	return binding
}

func TestRestoreRetainedReleasePreservesMetadataAndUsesActualPort(t *testing.T) {
	f := newReleaseFixture(t)

	result, err := f.store.Restore(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}

	binding := f.binding(t)
	if binding.Port != 3002 || result.LocalPort != 3002 || result.RestoredReleaseID != "previous" || !result.Fenced || result.DatabaseRestored || result.RookServiceStarted {
		t.Fatalf("binding=%+v result=%+v", binding, result)
	}
	var metadata struct {
		PreviousReleaseID    string               `json:"previousReleaseId"`
		FutureField          map[string]string    `json:"futureField"`
		RecoveryVerification RecoveryVerification `json:"recoveryVerification"`
	}
	if err := json.Unmarshal(binding.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.PreviousReleaseID != "current" || metadata.FutureField["keep"] != "opaque" || metadata.RecoveryVerification != result.Verification {
		t.Fatalf("metadata=%+v", metadata)
	}
	if !regexpUUID.MatchString(result.Verification.ID) {
		t.Fatalf("invalid verification UUID: %s", result.Verification.ID)
	}
	for _, name := range []string{"bindings.json", "recovery-fence", "releases/application/previous.json"} {
		info, err := os.Stat(filepath.Join(f.store.directory, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("%s mode=%v", name, info.Mode())
		}
	}
	bindings := NewBindings(f.store.directory)
	if err := bindings.Load(); err != nil {
		t.Fatal(err)
	}
	unrelated, ok := bindings.Get("unrelated")
	if !ok || unrelated.Port != 4000 {
		t.Fatal("unrelated binding changed")
	}
	if !reflect.DeepEqual(f.calls[len(f.calls)-1], []string{"docker", "stop", "--time", "60", currentContainer}) {
		t.Fatalf("last command: %v", f.calls[len(f.calls)-1])
	}
}

func TestFailedReadinessPreservesBindingAndStopsOnlyNewlyStartedContainers(t *testing.T) {
	f := newReleaseFixture(t)
	f.unhealthy = errors.New("readiness failed")
	f.runningWorker = true
	before := f.binding(t)

	_, err := f.store.Restore(context.Background(), f.options)

	if err == nil || !strings.Contains(err.Error(), "readiness failed") {
		t.Fatalf("error=%v", err)
	}
	if !reflect.DeepEqual(f.binding(t), before) {
		t.Fatal("failed readiness changed binding")
	}
	if !reflect.DeepEqual(f.calls[len(f.calls)-1], []string{"docker", "stop", "--time", "30", previousContainer}) {
		t.Fatalf("cleanup=%v", f.calls[len(f.calls)-1])
	}
	if _, err := os.Stat(filepath.Join(f.store.directory, "recovery-fence")); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreAcknowledgesEveryUncertainOperationBeforeContainerEffects(t *testing.T) {
	f := newReleaseFixture(t)
	if err := saveResult(filepath.Join(f.store.directory, "operations", "migration.json"), Result{Status: "interrupted"}); err != nil {
		t.Fatal(err)
	}

	_, err := f.store.Restore(context.Background(), f.options)

	if err == nil || !strings.Contains(err.Error(), "acknowledge each uncertain operation: migration") {
		t.Fatalf("error=%v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("unexpected effects: %v", f.calls)
	}
	f.options.AcknowledgeOperations = []string{"migration"}
	if _, err := f.store.Restore(context.Background(), f.options); err != nil {
		t.Fatal(err)
	}
}

func TestStaleCurrentReleaseCannotChangeContainers(t *testing.T) {
	f := newReleaseFixture(t)
	f.options.ExpectCurrent = "stale"

	_, err := f.store.Restore(context.Background(), f.options)

	if err == nil || !strings.Contains(err.Error(), "current release changed") {
		t.Fatalf("error=%v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("unexpected effects: %v", f.calls)
	}
}

func TestActiveServiceCannotBeginRestore(t *testing.T) {
	f := newReleaseFixture(t)
	f.active = "active"

	_, err := f.store.Restore(context.Background(), f.options)

	if err == nil || !strings.Contains(err.Error(), "stop the configured Rook") {
		t.Fatalf("error=%v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("unexpected effects: %v", f.calls)
	}
	if _, err := os.Stat(filepath.Join(f.store.directory, "recovery-fence")); !os.IsNotExist(err) {
		t.Fatalf("unexpected fence: %v", err)
	}
}

func TestMissingImageRefusesToStartRetainedContainers(t *testing.T) {
	f := newReleaseFixture(t)
	f.failImage = true
	before := f.binding(t)

	_, err := f.store.Restore(context.Background(), f.options)

	if err == nil || !strings.Contains(err.Error(), "image not found") {
		t.Fatalf("error=%v", err)
	}
	if len(f.calls) != 4 {
		t.Fatalf("unexpected effects: %v", f.calls)
	}
	if !reflect.DeepEqual(f.binding(t), before) {
		t.Fatal("missing image changed binding")
	}
}

func TestRestoreLockPreventsConcurrentEffects(t *testing.T) {
	f := newReleaseFixture(t)
	lock, err := os.OpenFile(filepath.Join(f.store.directory, "offline-recovery.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	_, err = f.store.Restore(context.Background(), f.options)

	if err == nil || !strings.Contains(err.Error(), "holds the lock") {
		t.Fatalf("error=%v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("unexpected effects: %v", f.calls)
	}
}

func TestCanceledReadinessStillCleansUpRetainedContainers(t *testing.T) {
	f := newReleaseFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.store.healthy = func(context.Context, int, string) error { cancel(); return ctx.Err() }

	_, err := f.store.Restore(ctx, f.options)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if f.cleanupCanceled {
		t.Fatal("cleanup inherited canceled context")
	}
	if !reflect.DeepEqual(f.calls[len(f.calls)-1], []string{"docker", "stop", "--time", "30", previousContainer, workerContainer}) {
		t.Fatalf("cleanup=%v", f.calls[len(f.calls)-1])
	}
}

func TestListAndInspectDoNotExecuteCommandsOrRewriteJournal(t *testing.T) {
	f := newReleaseFixture(t)
	path := filepath.Join(f.store.directory, "operations", "pending.json")
	if err := saveResult(path, Result{Status: "running"}); err != nil {
		t.Fatal(err)
	}

	inventory, err := f.store.List("application")
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := f.store.Inspect("application", "previous")
	if err != nil {
		t.Fatal(err)
	}

	if inventory.CurrentReleaseID != "current" || len(inventory.Releases) != 1 || len(inspection.Releases) != 1 || inspection.Releases[0].ReleaseID != "previous" {
		t.Fatalf("inventory=%+v inspection=%+v", inventory, inspection)
	}
	if !reflect.DeepEqual(inventory.UncertainOperations, []UncertainOperation{{ID: "pending", Status: "running"}}) {
		t.Fatalf("operations=%+v", inventory.UncertainOperations)
	}
	if len(f.calls) != 0 {
		t.Fatalf("unexpected commands: %v", f.calls)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"running"`) {
		t.Fatal("inspection rewrote journal")
	}
}

func TestInvalidReleaseIdentitiesCannotEscapeState(t *testing.T) {
	f := newReleaseFixture(t)
	for _, id := range []string{"../other", ".", "/tmp/other", "--flag", ""} {
		t.Run(id, func(t *testing.T) {
			if _, err := f.store.List(id); err == nil {
				t.Fatal("invalid deployment accepted")
			}
			if _, err := f.store.Inspect("application", id); err == nil {
				t.Fatal("invalid release accepted")
			}
			options := f.options
			options.Release = id
			if _, err := f.store.Restore(context.Background(), options); err == nil {
				t.Fatal("invalid restore accepted")
			}
		})
	}
	if len(f.calls) != 0 {
		t.Fatalf("unexpected effects: %v", f.calls)
	}
}

func TestReleaseReadinessUsesLocalHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_health" {
			t.Errorf("path=%s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(address.Port())
	if err != nil {
		t.Fatal(err)
	}

	if err := waitReleaseHealthy(context.Background(), port, "/_health"); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseReadinessDoesNotFollowRedirects(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed redirect") }))
	t.Cleanup(destination.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
	t.Cleanup(server.Close)
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(address.Port())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	t.Cleanup(cancel)

	err = waitReleaseHealthy(ctx, port, "/_health")

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
}

func TestInvalidRetainedStateCannotStartContainers(t *testing.T) {
	for name, change := range map[string]func(map[string]any){
		"wrong release":                func(value map[string]any) { value["releaseId"] = "other" },
		"invalid port":                 func(value map[string]any) { value["port"] = 0 },
		"external health URL":          func(value map[string]any) { value["healthPath"] = "http://example.test/health" },
		"protocol relative health URL": func(value map[string]any) { value["healthPath"] = "//example.test/health" },
		"web absent from roles":        func(value map[string]any) { value["roleContainers"] = []string{workerContainer} },
		"invalid container":            func(value map[string]any) { value["roleContainers"] = []string{previousContainer, "--bad"} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newReleaseFixture(t)
			path := filepath.Join(f.store.directory, "releases", "application", "previous.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if err := json.Unmarshal(data, &value); err != nil {
				t.Fatal(err)
			}
			change(value)
			data, err = json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}

			_, err = f.store.Restore(context.Background(), f.options)

			if err == nil {
				t.Fatal("invalid retained state accepted")
			}
			if len(f.calls) != 2 {
				t.Fatalf("container effects before validation: %v", f.calls)
			}
		})
	}
}

func TestCleanupFailureRetainsOriginalReadinessError(t *testing.T) {
	f := newReleaseFixture(t)
	readiness := errors.New("readiness failed")
	cleanup := errors.New("Docker unavailable during cleanup")
	f.unhealthy = readiness
	command := f.store.command
	f.store.command = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "docker" && args[0] == "stop" {
			return "", cleanup
		}
		return command(ctx, name, args...)
	}
	before := f.binding(t)

	_, err := f.store.Restore(context.Background(), f.options)

	if !errors.Is(err, readiness) || !errors.Is(err, cleanup) {
		t.Fatalf("lost error cause: %v", err)
	}
	if !reflect.DeepEqual(f.binding(t), before) {
		t.Fatal("failed cleanup changed binding")
	}
}
