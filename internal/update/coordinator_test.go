package update

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCoordinatorDefersBusyUpdatesAndExcludesNewCommands(t *testing.T) {
	u, gate, _, _ := newTestUpdater(t, false, true)
	client := socketClient(u.socket)
	defer client.CloseIdleConnections()
	ctx := context.Background()
	if !gate.BeginCommand() {
		t.Fatal("command refused")
	}
	if err := daemonAction(ctx, client, "prepare"); err == nil {
		t.Fatal("prepared while command active")
	}
	gate.EndCommand()
	if err := daemonAction(ctx, client, "prepare"); err != nil {
		t.Fatal(err)
	}
	if gate.BeginCommand() {
		t.Fatal("admitted command during update")
	}
	status, err := daemonStatus(ctx, client)
	if err != nil || status.Ready {
		t.Fatalf("prepared status: %+v, %v", status, err)
	}
	if err := daemonAction(ctx, client, "resume"); err != nil {
		t.Fatal(err)
	}
	if !gate.BeginCommand() {
		t.Fatal("did not resume commands")
	}
	gate.EndCommand()
	gate.DataReady(false)
	if err := daemonAction(ctx, client, "prepare"); err == nil {
		t.Fatal("prepared disconnected daemon")
	}
	info, err := os.Stat(u.socket)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("socket permissions: %v, %v", info, err)
	}
}

func TestCoordinatorDoesNotRemoveNonSocketPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "regular-file")
	os.WriteFile(path, []byte("keep"), 0600)
	gate := NewCoordinator("v0.2.1", func(context.Context) error { return nil }, func() {})
	if _, err := gate.Serve(context.Background(), path); err == nil {
		t.Fatal("accepted regular file socket path")
	}
	if data, _ := os.ReadFile(path); string(data) != "keep" {
		t.Fatal("removed regular file")
	}
}

func TestPreparationLeaseResumesAfterHelperDisappears(t *testing.T) {
	u, gate, _, _ := newTestUpdater(t, false, true)
	client := socketClient(u.socket)
	defer client.CloseIdleConnections()
	if err := daemonAction(context.Background(), client, "prepare"); err != nil {
		t.Fatal(err)
	}
	gate.mu.Lock()
	gate.lease.Reset(20 * time.Millisecond)
	gate.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for !gate.BeginCommand() {
		if time.Now().After(deadline) {
			t.Fatal("abandoned preparation blocked commands indefinitely")
		}
		time.Sleep(time.Millisecond)
	}
	gate.EndCommand()
}

func TestCoordinatorDoesNotReplaceLiveDaemonSocket(t *testing.T) {
	u, _, _, _ := newTestUpdater(t, false, true)
	gate := NewCoordinator("v0.2.2", func(context.Context) error { return nil }, func() {})
	if close, err := gate.Serve(context.Background(), u.socket); err == nil {
		close()
		t.Fatal("replaced live daemon socket")
	}
	client := socketClient(u.socket)
	defer client.CloseIdleConnections()
	status, err := daemonStatus(context.Background(), client)
	if err != nil || status.Version != "v0.2.1" {
		t.Fatalf("original socket lost: %+v, %v", status, err)
	}
}
