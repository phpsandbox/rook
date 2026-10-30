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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const repository = "phpsandbox/rook"
const BinaryPath = "/usr/local/bin/rook"

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

// Updater has fixed production paths and release origin. Tests substitute its
// I/O; no daemon message can choose an executable path or download origin.
type Updater struct {
	binary, directory, socket, arch string
	client                          *http.Client
	verify                          func(context.Context, string, string, string) error
	restart                         func(context.Context) error
	health                          func(context.Context, string) bool
}

func NewUpdater() *Updater {
	u := &Updater{
		binary: BinaryPath, directory: "/usr/local/bin/.rook-update", socket: SocketPath,
		arch: runtime.GOARCH,
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
	case "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com":
		return true
	}
	return false
}

func (u *Updater) fetch(ctx context.Context, address string, max int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/octet-stream")
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

func isPatchUpgrade(current, target string) bool {
	installed, currentOK := parseVersion(current)
	next, targetOK := parseVersion(target)
	return currentOK && targetOK && installed[0] == next[0] && installed[1] == next[1] && newer(next, installed)
}

func releaseAssetURL(target, name string) string {
	return "https://github.com/" + repository + "/releases/download/" + target + "/" + name
}

func (u *Updater) stage(ctx context.Context, target string) (string, error) {
	if _, ok := parseVersion(target); !ok {
		return "", errors.New("invalid release target")
	}
	name := "rook-linux-" + u.arch
	binary, err := u.fetch(ctx, releaseAssetURL(target, name), 32<<20)
	if err != nil {
		return "", err
	}
	checksum, err := u.fetch(ctx, releaseAssetURL(target, name+".sha256"), 4096)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(checksum))
	hash := sha256.Sum256(binary)
	if len(fields) != 2 || fields[1] != name || fields[0] != hex.EncodeToString(hash[:]) {
		return "", errors.New("release checksum mismatch")
	}
	bundle, err := u.fetch(ctx, releaseAssetURL(target, name+".sigstore.json"), 4<<20)
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
	if err := u.verify(ctx, staged, bundlePath, target); err != nil {
		return "", err
	}
	versionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(versionCtx, staged, "--version").Output()
	if err != nil || strings.TrimSpace(string(output)) != target {
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
	if status.Operation == nil || !status.Operation.Pending() {
		return nil
	}
	operation := *status.Operation
	if !operation.valid() {
		return errors.New("invalid update request")
	}
	if operation.Trigger == "automatic" && !status.AutoUpdate {
		return reportResult(ctx, daemon, operation, "failed")
	}
	if status.Busy {
		return reportResult(ctx, daemon, operation, "waiting")
	}

	replacementStarted := false
	defer func() {
		if applyErr != nil && !replacementStarted {
			reportCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			applyErr = errors.Join(applyErr, reportResult(reportCtx, daemon, operation, "failed"))
		}
	}()
	if err := reportResult(ctx, daemon, operation, "verifying"); err != nil {
		return err
	}
	failed, err := os.ReadFile(filepath.Join(u.directory, "failed-versions"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	target := operation.TargetVersion
	if !isPatchUpgrade(status.Version, target) || slices.Contains(strings.Fields(string(failed)), target) {
		return errors.New("requested release is ineligible or was previously rolled back")
	}
	defer os.Remove(filepath.Join(u.directory, "candidate"))
	defer os.Remove(filepath.Join(u.directory, "candidate.sigstore.json"))
	staged, err := u.stage(ctx, target)
	if err != nil {
		return err
	}
	// Downloads and cryptographic verification happen before interrupting traffic.
	if err := daemonAction(ctx, daemon, "prepare"); err != nil {
		if errors.Is(err, errUpdateDeferred) {
			fmt.Println("rook update: agent busy or disconnected; retrying at the next check")
			return reportResult(ctx, daemon, operation, "waiting")
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
	if err != nil || prepared.Version != status.Version || prepared.Ready || (operation.Trigger == "automatic" && !prepared.AutoUpdate) {
		return errors.New("daemon changed while preparing update")
	}
	fmt.Printf("rook auto-update: installing %s (current %s)\n", target, status.Version)
	old, err := os.ReadFile(u.binary)
	if err != nil {
		return err
	}
	if err := durableWrite(filepath.Join(u.directory, "previous"), old, 0755); err != nil {
		return err
	}
	pending := pendingUpdate{Version: target, Previous: status.Version, Operation: &operation}
	if err := reportResult(ctx, daemon, operation, "installing"); err != nil {
		return err
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
	if err := u.restart(finishCtx); err == nil && u.health(finishCtx, target) {
		fmt.Printf("rook auto-update: %s connected and healthy\n", target)
		return u.complete(finishCtx, pending, "succeeded")
	}
	if err := u.rollback(finishCtx, pending); err != nil {
		return err
	}
	return fmt.Errorf("update to %s failed health check; restored %s", target, status.Version)
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
