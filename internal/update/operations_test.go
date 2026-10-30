package update

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

const testOperationID = "b513ad9c-a0d9-4bb0-a8f8-fb7f529d62c9"

func TestRequestedUpdateSurvivesRestartAndIsIdempotent(t *testing.T) {
	directory := t.TempDir()
	operations, err := OpenOperations(directory)
	if err != nil {
		t.Fatal(err)
	}

	requested, err := operations.Request(testOperationID, "v0.2.2", "v0.2.1", "manual")
	if err != nil {
		t.Fatal(err)
	}
	restored, err := OpenOperations(directory)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := restored.Request(testOperationID, "v0.2.2", "v0.2.1", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if duplicate != requested {
		t.Fatalf("duplicate changed request: %+v", duplicate)
	}
	if _, err := restored.Request(testOperationID, "v0.2.3", "v0.2.1", "manual"); err == nil {
		t.Fatal("accepted changed identity")
	}
	if _, err := restored.Request("e43f705c-bc67-40de-bfb1-12073cf4203b", "v0.2.3", "v0.2.1", "manual"); err == nil {
		t.Fatal("accepted concurrent update")
	}

	requested.Status = "succeeded"
	if err := restored.Report(requested); err != nil {
		t.Fatal(err)
	}
	final, err := OpenOperations(directory)
	if err != nil {
		t.Fatal(err)
	}
	if operation := final.Snapshot(); operation == nil || operation.Status != "succeeded" {
		t.Fatalf("completion lost: %+v", operation)
	}
	requested.Status = "installing"
	if err := final.Report(requested); err == nil {
		t.Fatal("reopened completed update")
	}
}

func TestRequestRejectsDowngradeAndUntrustedTarget(t *testing.T) {
	for _, target := range []string{"v0.2.0", "v0.2.1", "v0.3.0", "v1.2.2", "v0.2.2-rc.1", "https://example.test/rook", "latest"} {
		t.Run(target, func(t *testing.T) {
			operations, err := OpenOperations(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := operations.Request(testOperationID, target, "v0.2.1", "manual"); err == nil {
				t.Fatal("accepted ineligible target")
			}
		})
	}
}

func TestManualUpdateDoesNotEnableAutomaticUpdates(t *testing.T) {
	updater, gate, verifies, restarts := newTestUpdater(t, false, true)
	gate.autoUpdate = false
	if err := gate.Enable(filepath.Dir(updater.binary), false); err != nil {
		t.Fatal(err)
	}

	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *verifies != 0 || *restarts != 0 {
		t.Fatal("updated without a request or opt-in")
	}

	if _, err := gate.Request(testOperationID, "v0.2.2", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *verifies != 1 || *restarts != 1 {
		t.Fatalf("unexpected update count: %d, %d", *verifies, *restarts)
	}
	if operation := gate.Operation(); operation == nil || operation.Status != "succeeded" {
		t.Fatalf("completion missing: %+v", operation)
	}
	if gate.autoUpdate {
		t.Fatal("manual update enabled automatic updates")
	}
	if _, err := os.Stat(filepath.Join(updater.directory, "pending.json")); !os.IsNotExist(err) {
		t.Fatalf("journal retained: %v", err)
	}
}

func TestManualUpdateRejectsTamperedReleaseWithoutRestart(t *testing.T) {
	updater, gate, _, restarts := newTestUpdater(t, true, true)
	if err := gate.Enable(filepath.Dir(updater.binary), false); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Request(testOperationID, "v0.2.2", "manual"); err != nil {
		t.Fatal(err)
	}

	if err := updater.Apply(context.Background()); err == nil {
		t.Fatal("accepted invalid provenance")
	}
	if *restarts != 0 {
		t.Fatal("restarted before verification")
	}
	if operation := gate.Operation(); operation == nil || operation.Status != "failed" {
		t.Fatalf("failure missing: %+v", operation)
	}
}

func TestExplicitRequestCannotBypassFailedVersionSuppression(t *testing.T) {
	updater, gate, _, restarts := newTestUpdater(t, false, true)
	requestTestUpdate(t, gate)
	if err := os.WriteFile(filepath.Join(updater.directory, "failed-versions"), []byte("v0.2.2\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := updater.Apply(context.Background()); err == nil {
		t.Fatal("retried a failed release")
	}
	if *restarts != 0 {
		t.Fatal("restarted a failed release")
	}
}

func TestManualUpdateWaitsForActiveCommandsBeforeDownloading(t *testing.T) {
	updater, gate, verifies, restarts := newTestUpdater(t, false, true)
	if err := gate.Enable(filepath.Dir(updater.binary), false); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Request(testOperationID, "v0.2.2", "manual"); err != nil {
		t.Fatal(err)
	}
	transport := updater.client.Transport
	downloads := 0
	updater.client.Transport = transportFunc(func(request *http.Request) (*http.Response, error) {
		downloads++
		return transport.RoundTrip(request)
	})
	if !gate.BeginCommand() {
		t.Fatal("could not start host work")
	}
	t.Cleanup(gate.EndCommand)

	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}

	if downloads != 0 || *verifies != 0 || *restarts != 0 {
		t.Fatal("downloaded or installed while busy")
	}
	if operation := gate.Operation(); operation == nil || operation.Status != "waiting" {
		t.Fatalf("not waiting: %+v", operation)
	}
}

func TestLostCompletionDeliveryDoesNotRepeatBinaryReplacement(t *testing.T) {
	updater, gate, _, restarts := newTestUpdater(t, false, true)
	if err := gate.Enable(filepath.Dir(updater.binary), false); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Request(testOperationID, "v0.2.2", "manual"); err != nil {
		t.Fatal(err)
	}
	updater.restart = func(context.Context) error {
		*restarts++
		gate.operations = nil
		return nil
	}

	if err := updater.Apply(context.Background()); err == nil {
		t.Fatal("expected completion delivery failure")
	}
	if *restarts != 1 {
		t.Fatalf("unexpected restarts: %d", *restarts)
	}

	if err := gate.Enable(filepath.Dir(updater.binary), false); err != nil {
		t.Fatal(err)
	}
	updater.health = func(context.Context, string) bool { return false }
	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}

	if *restarts != 1 {
		t.Fatalf("repeated replacement after completed update: %d", *restarts)
	}
	if operation := gate.Operation(); operation == nil || operation.Status != "succeeded" {
		t.Fatalf("completion missing: %+v", operation)
	}
	if _, err := os.Stat(filepath.Join(updater.directory, "pending.json")); !os.IsNotExist(err) {
		t.Fatalf("journal retained: %v", err)
	}
}

func TestAutomaticUpdatesRequireAnExactPlatformRequest(t *testing.T) {
	updater, gate, verifies, restarts := newTestUpdater(t, false, true)
	original := updater.client.Transport
	downloads := 0
	updater.client.Transport = transportFunc(func(request *http.Request) (*http.Response, error) {
		downloads++
		return original.RoundTrip(request)
	})

	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if downloads != 0 || *verifies != 0 || *restarts != 0 {
		t.Fatal("automatic policy performed work without a platform request")
	}

	if _, err := gate.Request(testOperationID, "v0.2.2", "automatic"); err != nil {
		t.Fatal(err)
	}
	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}

	operation := gate.Operation()
	if operation == nil || operation.Trigger != "automatic" || operation.Status != "succeeded" {
		t.Fatalf("automatic operation was not recorded: %+v", operation)
	}
	if downloads != 3 || *verifies != 1 || *restarts != 1 {
		t.Fatalf("unexpected work: downloads=%d, verifies=%d, restarts=%d", downloads, *verifies, *restarts)
	}
}

func TestManualPolicyRejectsAutomaticRequests(t *testing.T) {
	updater, gate, _, _ := newTestUpdater(t, false, true)
	if err := gate.Enable(filepath.Dir(updater.binary), false); err != nil {
		t.Fatal(err)
	}

	if _, err := gate.Request(testOperationID, "v0.2.2", "automatic"); err == nil || err.Error() != "automatic updates are disabled" {
		t.Fatalf("automatic request was not rejected: %v", err)
	}
	if gate.Operation() != nil {
		t.Fatal("rejected automatic request was persisted")
	}
	if _, err := gate.Request(testOperationID, "v0.2.2", "manual"); err != nil {
		t.Fatal(err)
	}
}

func TestHelperRechecksAutomaticPolicyBeforeDownloading(t *testing.T) {
	updater, gate, verifies, restarts := newTestUpdater(t, false, true)
	if _, err := gate.Request(testOperationID, "v0.2.2", "automatic"); err != nil {
		t.Fatal(err)
	}
	if err := gate.Enable(filepath.Dir(updater.binary), false); err != nil {
		t.Fatal(err)
	}
	updater.client.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("downloaded an automatic update after opt-out")
		return nil, nil
	})

	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}

	operation := gate.Operation()
	if operation == nil || operation.Status != "failed" || operation.Trigger != "automatic" {
		t.Fatalf("automatic cancellation was not recorded: %+v", operation)
	}
	if *verifies != 0 || *restarts != 0 {
		t.Fatal("installed an automatic update after opt-out")
	}
}

func TestRequestsRequireAnExplicitTrigger(t *testing.T) {
	for _, trigger := range []string{"", "unknown"} {
		t.Run(trigger, func(t *testing.T) {
			operations, err := OpenOperations(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}

			_, err = operations.Request(testOperationID, "v0.2.2", "v0.2.1", trigger)

			if err == nil || err.Error() != "invalid update request" {
				t.Fatalf("missing or invalid trigger was accepted: %v", err)
			}
			if operations.Snapshot() != nil {
				t.Fatal("invalid request was persisted")
			}
		})
	}
}
