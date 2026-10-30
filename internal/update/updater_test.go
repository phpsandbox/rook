package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeRelease(tag string) release {
	r := release{Tag: tag}
	for _, suffix := range []string{"", ".sha256", ".sigstore.json"} {
		name := "rook-linux-amd64" + suffix
		r.Assets = append(r.Assets, struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}{name, "https://github.com/" + repository + "/releases/download/" + tag + "/" + name})
	}
	return r
}

func TestSelectReleaseOnlyNewCompleteStablePatches(t *testing.T) {
	releases := []release{makeRelease("v0.2.0"), makeRelease("v0.2.2"), makeRelease("v0.2.3"), makeRelease("v0.3.0"), makeRelease("v1.2.9"), makeRelease("v0.2.5"), makeRelease("v0.2.6"), makeRelease("v0.2.7"), makeRelease("v0.2.8-rc.1")}
	releases[5].Prerelease = true
	releases[6].Draft = true
	releases[7].Assets = releases[7].Assets[:2]
	selected, ok := selectRelease(releases, "v0.2.1", "v0.2.3", "amd64")
	if !ok || selected.Tag != "v0.2.2" {
		t.Fatalf("selected %+v, %v", selected, ok)
	}
	for _, current := range []string{"dev", "v0.2.99", "v0.2.1-rc.1", "v0.02.1", "v0.2.18446744073709551616"} {
		if _, ok := selectRelease(releases, current, "", "amd64"); ok {
			t.Fatalf("unexpected update from %q", current)
		}
	}
	if _, ok := selectRelease(releases, "v0.2.1", "", "arm64"); ok {
		t.Fatal("selected release without architecture assets")
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newTestUpdater(t *testing.T, signatureError bool, healthy bool) (*Updater, *Coordinator, *int, *int) {
	t.Helper()
	directory := t.TempDir()
	os.Chmod(directory, 0700)
	socketDir, err := os.MkdirTemp("/tmp", "rook-update-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "agent.sock")
	gate := NewCoordinator("v0.2.1", func(context.Context) error { return nil }, func() {})
	gate.autoUpdate = true
	gate.ControlReady(true)
	gate.DataReady(true)
	closeGate, err := gate.Serve(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeGate)
	binary := []byte("#!/bin/sh\nprintf '%s\\n' 'v0.2.2'\n")
	hash := sha256.Sum256(binary)
	candidate := makeRelease("v0.2.2")
	metadata, _ := json.Marshal([]release{candidate})
	data := map[string][]byte{
		releaseAPI:              metadata,
		candidate.Assets[0].URL: binary,
		candidate.Assets[1].URL: []byte(fmt.Sprintf("%x  rook-linux-amd64\n", hash)),
		candidate.Assets[2].URL: []byte("signed bundle fixture"),
	}
	verifies, restarts := new(int), new(int)
	u := &Updater{
		binary: filepath.Join(directory, "rook"), directory: directory, socket: socket, api: releaseAPI, arch: "amd64",
		client: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			body, ok := data[r.URL.String()]
			if !ok {
				t.Errorf("unexpected URL: %s", r.URL)
				body = nil
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
		})},
		verify: func(ctx context.Context, binary, bundle, tag string) error {
			*verifies++
			if tag != "v0.2.2" {
				t.Errorf("verification tag = %s", tag)
			}
			if signatureError {
				return fmt.Errorf("invalid signature")
			}
			return nil
		},
		restart: func(context.Context) error { *restarts++; return nil },
		health:  func(ctx context.Context, version string) bool { return version == "v0.2.1" || healthy },
	}
	if err := os.WriteFile(u.binary, []byte("old binary"), 0755); err != nil {
		t.Fatal(err)
	}
	return u, gate, verifies, restarts
}

func TestApplyVerifiedUpdateAndRollback(t *testing.T) {
	for _, healthy := range []bool{true, false} {
		t.Run(fmt.Sprint(healthy), func(t *testing.T) {
			u, _, verifies, restarts := newTestUpdater(t, false, healthy)
			err := u.Apply(context.Background())
			data, readErr := os.ReadFile(u.binary)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if healthy {
				if err != nil || !strings.Contains(string(data), "v0.2.2") || *restarts != 1 {
					t.Fatalf("update: %v, %q, restarts=%d", err, data, *restarts)
				}
			} else {
				if err == nil || string(data) != "old binary" || *restarts != 2 {
					t.Fatalf("rollback: %v, %q, restarts=%d", err, data, *restarts)
				}
				failed, _ := os.ReadFile(filepath.Join(u.directory, "failed-versions"))
				if strings.TrimSpace(string(failed)) != "v0.2.2" {
					t.Fatalf("failed version not recorded: %q", failed)
				}
				if err := u.Apply(context.Background()); err != nil || *restarts != 2 {
					t.Fatalf("retried failed version: %v, %d", err, *restarts)
				}
			}
			if *verifies != 1 {
				t.Fatalf("verification count: %d", *verifies)
			}
			if _, err := os.Stat(filepath.Join(u.directory, "pending.json")); !os.IsNotExist(err) {
				t.Fatalf("journal remains: %v", err)
			}
			backup, _ := os.ReadFile(filepath.Join(u.directory, "previous"))
			if string(backup) != "old binary" {
				t.Fatal("missing rollback backup")
			}
		})
	}
}

func TestApplyRejectsUnverifiedAndBusyUpdates(t *testing.T) {
	for _, signatureError := range []bool{true, false} {
		t.Run(fmt.Sprint(signatureError), func(t *testing.T) {
			u, gate, _, restarts := newTestUpdater(t, signatureError, false)
			if !signatureError {
				gate.BeginCommand()
				defer gate.EndCommand()
			}
			err := u.Apply(context.Background())
			if signatureError && err == nil {
				t.Fatal("accepted unsafe update")
			}
			if !signatureError && err != nil {
				t.Fatalf("busy update was not deferred: %v", err)
			}
			old, _ := os.ReadFile(u.binary)
			if string(old) != "old binary" || *restarts != 0 {
				t.Fatal("changed binary before verification/preparation")
			}
		})
	}
}

func TestRecoverInterruptedReplacement(t *testing.T) {
	u, _, _, restarts := newTestUpdater(t, false, false)
	os.WriteFile(filepath.Join(u.directory, "previous"), []byte("previous binary"), 0755)
	os.WriteFile(u.binary, []byte("interrupted candidate"), 0755)
	journal, _ := json.Marshal(pendingUpdate{Version: "v0.2.2", Previous: "v0.2.1"})
	os.WriteFile(filepath.Join(u.directory, "pending.json"), journal, 0600)
	if err := u.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	actual, _ := os.ReadFile(u.binary)
	if string(actual) != "previous binary" || *restarts != 1 {
		t.Fatalf("did not recover: %q, %d", actual, *restarts)
	}
}

func TestDisabledAgentMakesNoNetworkRequests(t *testing.T) {
	u, _, _, restarts := newTestUpdater(t, false, true)
	u.socket = filepath.Join(u.directory, "missing.sock")
	u.client.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("disabled updater made network request")
		return nil, nil
	})
	if err := u.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *restarts != 0 {
		t.Fatal("disabled updater restarted daemon")
	}
}

func TestDownloadLimitsAndOrigins(t *testing.T) {
	u := NewUpdater()
	for _, address := range []string{"http://github.com/x", "https://evil.test/x", "https://github.com.evil.test/x", "https://user@github.com/x", "https://github.com:443/x"} {
		parsed, _ := url.Parse(address)
		if err := u.client.CheckRedirect(&http.Request{URL: parsed}, nil); err == nil {
			t.Fatalf("allowed %s", address)
		}
	}
	u.client = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("too much"))}, nil
	})}
	if _, err := u.fetch(context.Background(), releaseAPI, 2); err == nil {
		t.Fatal("accepted oversized download")
	}
}

func TestChecksumFailureDoesNotExecuteOrReplaceBinary(t *testing.T) {
	u, _, verifies, restarts := newTestUpdater(t, false, true)
	original := u.client.Transport
	u.client.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("0", 64) + "  rook-linux-amd64\n"))}, nil
		}
		return original.RoundTrip(r)
	})
	if err := u.Apply(context.Background()); err == nil {
		t.Fatal("accepted invalid checksum")
	}
	data, _ := os.ReadFile(u.binary)
	if string(data) != "old binary" || *verifies != 0 || *restarts != 0 {
		t.Fatal("used corrupt binary")
	}
}

func TestRejectUnexpectedAssetURL(t *testing.T) {
	u, _, verifies, restarts := newTestUpdater(t, false, true)
	candidate := makeRelease("v0.2.2")
	candidate.Assets[0].URL = "https://evil.test/rook"
	if _, err := u.stage(context.Background(), candidate); err == nil {
		t.Fatal("allowed attacker-controlled download URL")
	}
	if *verifies != 0 || *restarts != 0 {
		t.Fatal("used untrusted origin")
	}
}

func TestRejectSymlinkUpdaterDirectory(t *testing.T) {
	u, _, _, _ := newTestUpdater(t, false, true)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(u.directory, link); err != nil {
		t.Fatal(err)
	}
	u.directory = link
	if err := u.Apply(context.Background()); err == nil {
		t.Fatal("used symlink updater directory")
	}
}

func TestRollbackDoesNotLoopWhenPreviousAgentIsOffline(t *testing.T) {
	u, gate, _, restarts := newTestUpdater(t, false, false)
	u.health = func(context.Context, string) bool { return false }
	if err := u.Apply(context.Background()); err == nil {
		t.Fatal("expected health failure")
	}
	if _, err := os.Stat(filepath.Join(u.directory, "pending.json")); !os.IsNotExist(err) {
		t.Fatal("completed rollback journal retained")
	}
	gate.DataReady(false)
	if err := u.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *restarts != 2 {
		t.Fatal("restarted restored agent repeatedly")
	}
}

func TestRollbackRestartFailureCanBeRecovered(t *testing.T) {
	u, _, _, restarts := newTestUpdater(t, false, false)
	u.restart = func(context.Context) error {
		*restarts++
		if *restarts == 2 {
			return fmt.Errorf("systemd temporarily unavailable")
		}
		return nil
	}
	if err := u.Apply(context.Background()); err == nil {
		t.Fatal("ignored rollback restart failure")
	}
	if _, err := os.Stat(filepath.Join(u.directory, "pending.json")); err != nil {
		t.Fatal("lost pending recovery journal")
	}
	if err := u.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *restarts != 3 {
		t.Fatalf("recovery restart count %d", *restarts)
	}
}
