package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const repository = "phpsandbox/rook"
const BinaryPath = "/usr/local/bin/rook"
const releaseAPI = "https://api.github.com/repos/" + repository + "/releases?per_page=100"

var versionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type semver [3]uint64

func parseVersion(value string) (semver, bool) {
	match := versionPattern.FindStringSubmatch(value)
	if match == nil {
		return semver{}, false
	}
	var version semver
	for i := range version {
		number, err := strconv.ParseUint(match[i+1], 10, 64)
		if err != nil {
			return semver{}, false
		}
		version[i] = number
	}
	return version, true
}

type release struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Updater has fixed production paths and release origin. Tests substitute its
// I/O; no daemon message can choose an executable path or download origin.
type Updater struct {
	binary, directory, socket, api, arch string
	client                               *http.Client
	verify                               func(context.Context, string, string, string) error
	restart                              func(context.Context) error
	health                               func(context.Context, string) bool
}

func NewUpdater() *Updater {
	u := &Updater{
		binary: BinaryPath, directory: "/usr/local/bin/.rook-update", socket: SocketPath,
		api: releaseAPI, arch: runtime.GOARCH,
		client: &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 || !allowedDownloadURL(request.URL) {
				return errors.New("untrusted update redirect")
			}
			return nil
		}},
	}
	u.verify = verifyAttestation
	u.restart = func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, "/usr/bin/systemctl", "restart", "rook.service").CombinedOutput()
		if err != nil {
			return fmt.Errorf("restart rook: %w: %s", err, output)
		}
		return nil
	}
	u.health = func(ctx context.Context, version string) bool {
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		client := socketClient(u.socket)
		defer client.CloseIdleConnections()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var readySince time.Time
		for {
			status, err := daemonStatus(ctx, client)
			if err == nil && status.Ready && status.Version == version {
				if readySince.IsZero() {
					readySince = time.Now()
				}
				if time.Since(readySince) >= 5*time.Second {
					return true
				}
			} else {
				readySince = time.Time{}
			}
			select {
			case <-ctx.Done():
				return false
			case <-ticker.C:
			}
		}
	}
	return u
}

func allowedDownloadURL(value *url.URL) bool {
	if value.Scheme != "https" || value.User != nil || value.Port() != "" {
		return false
	}
	switch value.Hostname() {
	case "api.github.com", "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com":
		return true
	}
	return false
}

func (u *Updater) fetch(ctx context.Context, address string, max int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	if address == u.api {
		request.Header.Set("Accept", "application/vnd.github+json")
	} else {
		request.Header.Set("Accept", "application/octet-stream")
	}
	request.Header.Set("User-Agent", "rook-auto-update")
	response, err := u.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, errors.New("update download exceeds size limit")
	}
	return body, nil
}

func selectRelease(releases []release, current, failed, arch string) (release, bool) {
	version, ok := parseVersion(current)
	if !ok {
		return release{}, false
	}
	var selected release
	best := version[2]
	for _, candidate := range releases {
		next, ok := parseVersion(candidate.Tag)
		if !ok || !eligibleRelease(candidate, failed, arch) || next[0] != version[0] || next[1] != version[1] || next[2] <= best {
			continue
		}
		selected, best = candidate, next[2]
	}
	return selected, selected.Tag != ""
}

func (u *Updater) stage(ctx context.Context, candidate release) (string, error) {
	name := "rook-linux-" + u.arch
	paths := map[string]string{}
	for _, asset := range candidate.Assets {
		if asset.Name != name && asset.Name != name+".sha256" && asset.Name != name+".sigstore.json" {
			continue
		}
		expected := "https://github.com/" + repository + "/releases/download/" + candidate.Tag + "/" + asset.Name
		if asset.URL != expected {
			return "", errors.New("unexpected release asset URL")
		}
		paths[asset.Name] = asset.URL
	}
	binary, err := u.fetch(ctx, paths[name], 32<<20)
	if err != nil {
		return "", err
	}
	checksum, err := u.fetch(ctx, paths[name+".sha256"], 4096)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(checksum))
	hash := sha256.Sum256(binary)
	if len(fields) != 2 || fields[1] != name || fields[0] != hex.EncodeToString(hash[:]) {
		return "", errors.New("release checksum mismatch")
	}
	bundle, err := u.fetch(ctx, paths[name+".sigstore.json"], 4<<20)
	if err != nil {
		return "", err
	}
	staged := filepath.Join(u.directory, "candidate")
	bundlePath := filepath.Join(u.directory, "candidate.sigstore.json")
	if err := durableWrite(staged, binary, 0700); err != nil {
		return "", err
	}
	if err := durableWrite(bundlePath, bundle, 0600); err != nil {
		return "", err
	}
	if err := u.verify(ctx, staged, bundlePath, candidate.Tag); err != nil {
		return "", err
	}
	versionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(versionCtx, staged, "--version").Output()
	if err != nil || strings.TrimSpace(string(output)) != candidate.Tag {
		return "", errors.New("verified binary reports an unexpected version")
	}
	return staged, nil
}

type pendingUpdate struct {
	Version   string     `json:"version"`
	Previous  string     `json:"previous"`
	Operation *Operation `json:"operation,omitempty"`
	Outcome   string     `json:"outcome,omitempty"`
}

// Apply is run by the root systemd helper. A missing coordinator socket means no
// network request is made. The journal recovers an interrupted replacement.
func (u *Updater) Apply(ctx context.Context) (applyErr error) {
	if err := os.MkdirAll(u.directory, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(u.directory)
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || owner.Uid != uint32(os.Geteuid()) {
		return errors.New("unsafe updater directory ownership or permissions")
	}
	lock, err := os.OpenFile(filepath.Join(u.directory, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("updater already running: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := u.recover(ctx); err != nil {
		return err
	}
	if _, err := os.Lstat(u.socket); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	daemon := socketClient(u.socket)
	defer daemon.CloseIdleConnections()
	status, err := daemonStatus(ctx, daemon)
	if err != nil {
		return err
	}
	if !status.Ready {
		return nil
	}
	if _, ok := parseVersion(status.Version); !ok {
		return nil
	} // Development builds never self-update.
	manual := status.Operation != nil && status.Operation.Pending()
	if status.Busy {
		if manual {
			return reportResult(ctx, daemon, *status.Operation, "waiting")
		}
		return nil
	}
	if !manual {
		if !status.AutoUpdate {
			return nil
		}
		checked, err := os.Stat(filepath.Join(u.directory, "last-check"))
		if err == nil && time.Since(checked.ModTime()) < 24*time.Hour {
			return nil
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	replacementStarted := false
	if manual {
		defer func() {
			if applyErr != nil && !replacementStarted {
				reportCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				applyErr = errors.Join(applyErr, reportResult(reportCtx, daemon, *status.Operation, "failed"))
			}
		}()
		if err := reportResult(ctx, daemon, *status.Operation, "verifying"); err != nil {
			return err
		}
	}
	if !manual {
		if err := durableWrite(filepath.Join(u.directory, "last-check"), []byte(time.Now().UTC().Format(time.RFC3339)), 0600); err != nil {
			return err
		}
	}
	data, err := u.fetch(ctx, u.api, 4<<20)
	if err != nil {
		return err
	}
	var releases []release
	if err := json.Unmarshal(data, &releases); err != nil {
		return err
	}
	failed, err := os.ReadFile(filepath.Join(u.directory, "failed-versions"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	candidate, ok := selectRelease(releases, status.Version, strings.TrimSpace(string(failed)), u.arch)
	if manual {
		candidate, ok = selectRequestedRelease(releases, status.Version, status.Operation.TargetVersion, strings.TrimSpace(string(failed)), u.arch)
		if !ok {
			return errors.New("requested release is unavailable or ineligible")
		}
	}
	if !ok {
		return nil
	}
	defer os.Remove(filepath.Join(u.directory, "candidate"))
	defer os.Remove(filepath.Join(u.directory, "candidate.sigstore.json"))
	staged, err := u.stage(ctx, candidate)
	if err != nil {
		return err
	}
	// Downloads and cryptographic verification happen before interrupting traffic.
	if err := daemonAction(ctx, daemon, "prepare"); err != nil {
		if errors.Is(err, errUpdateDeferred) {
			fmt.Println("rook update: agent busy or disconnected; retrying at the next check")
			if !manual {
				if err := os.Remove(filepath.Join(u.directory, "last-check")); err != nil {
					return err
				}
			}
			if manual {
				return reportResult(ctx, daemon, *status.Operation, "waiting")
			}
			return nil
		}
		return fmt.Errorf("update deferred: %w", err)
	}
	defer func() {
		resumeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = daemonAction(resumeCtx, daemon, "resume")
	}()
	// With admission stopped, check that the same daemon is still running.
	prepared, err := daemonStatus(ctx, daemon)
	if err != nil || prepared.Version != status.Version || prepared.Ready {
		return errors.New("daemon changed while preparing update")
	}
	fmt.Printf("rook auto-update: installing %s (current %s)\n", candidate.Tag, status.Version)
	old, err := os.ReadFile(u.binary)
	if err != nil {
		return err
	}
	if err := durableWrite(filepath.Join(u.directory, "previous"), old, 0755); err != nil {
		return err
	}
	pending := pendingUpdate{Version: candidate.Tag, Previous: status.Version, Operation: status.Operation}
	if !manual {
		pending.Operation = nil
	}
	if manual {
		if err := reportResult(ctx, daemon, *status.Operation, "installing"); err != nil {
			return err
		}
	}
	journal, _ := json.Marshal(pending)
	if err := durableWrite(filepath.Join(u.directory, "pending.json"), journal, 0600); err != nil {
		return err
	}
	replacementStarted = true
	if err := os.Chmod(staged, 0755); err != nil {
		return err
	}
	if err := os.Rename(staged, u.binary); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(u.binary)); err != nil {
		return err
	}
	// Rollback must still run if the helper receives a termination signal.
	finishCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := u.restart(finishCtx); err == nil && u.health(finishCtx, candidate.Tag) {
		fmt.Printf("rook auto-update: %s connected and healthy\n", candidate.Tag)
		return u.complete(finishCtx, pending, "succeeded")
	}
	if err := u.rollback(finishCtx, pending); err != nil {
		return err
	}
	return fmt.Errorf("update to %s failed health check; restored %s", candidate.Tag, status.Version)
}

func (u *Updater) recover(ctx context.Context) error {
	data, err := os.ReadFile(filepath.Join(u.directory, "pending.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var pending pendingUpdate
	if err := json.Unmarshal(data, &pending); err != nil {
		return err
	}
	if _, ok := parseVersion(pending.Version); !ok {
		return errors.New("invalid pending update version")
	}
	if _, ok := parseVersion(pending.Previous); !ok {
		return errors.New("invalid previous update version")
	}
	if pending.Outcome == "succeeded" || pending.Outcome == "rolled_back" {
		return u.complete(ctx, pending, pending.Outcome)
	}
	if u.health(ctx, pending.Version) {
		return u.complete(ctx, pending, "succeeded")
	}
	return u.rollback(ctx, pending)
}

func (u *Updater) rollback(ctx context.Context, pending pendingUpdate) error {
	previous, err := os.ReadFile(filepath.Join(u.directory, "previous"))
	if err != nil {
		return fmt.Errorf("load rollback binary: %w", err)
	}
	if err := durableWrite(u.binary, previous, 0755); err != nil {
		return fmt.Errorf("restore rollback binary: %w", err)
	}
	failedPath := filepath.Join(u.directory, "failed-versions")
	failed, err := os.ReadFile(failedPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	failed = append(failed, []byte(pending.Version+"\n")...)
	if err := durableWrite(failedPath, failed, 0600); err != nil {
		return err
	}
	if err := u.restart(ctx); err != nil {
		return fmt.Errorf("restart restored binary: %w", err)
	}
	// Replacement is now rolled back even if an unrelated network outage keeps
	// the old agent offline. Do not restart it again at every scheduled check.
	if err := u.complete(ctx, pending, "rolled_back"); err != nil {
		return err
	}
	if !u.health(ctx, pending.Previous) {
		return errors.New("previous binary restored but agent is not connected; inspect rook.service")
	}
	return nil
}

func (u *Updater) clearPending() error {
	if err := os.Remove(filepath.Join(u.directory, "pending.json")); err != nil {
		return err
	}
	return syncDirectory(u.directory)
}

func durableWrite(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".rook-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (u *Updater) complete(ctx context.Context, pending pendingUpdate, status string) error {
	if pending.Operation != nil {
		pending.Outcome = status
		data, err := json.Marshal(pending)
		if err != nil {
			return err
		}
		if err := durableWrite(filepath.Join(u.directory, "pending.json"), data, 0600); err != nil {
			return err
		}
		client := socketClient(u.socket)
		defer client.CloseIdleConnections()
		if err := reportResult(ctx, client, *pending.Operation, status); err != nil {
			return err
		}
	}
	return u.clearPending()
}

func selectRequestedRelease(releases []release, current, target, failed, arch string) (release, bool) {
	for _, candidate := range releases {
		if candidate.Tag == target {
			return selectRelease([]release{candidate}, current, failed, arch)
		}
	}
	return release{}, false
}

func eligibleRelease(candidate release, failed, arch string) bool {
	if candidate.Draft || candidate.Prerelease {
		return false
	}
	for _, blocked := range strings.Fields(failed) {
		if blocked == candidate.Tag {
			return false
		}
	}
	names := map[string]bool{}
	for _, asset := range candidate.Assets {
		names[asset.Name] = true
	}
	name := "rook-linux-" + arch
	return names[name] && names[name+".sha256"] && names[name+".sigstore.json"]
}
