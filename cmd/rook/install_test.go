package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Execute the real installer functions against a temporary filesystem and a
// recording systemctl, without installing services or touching host credentials.
func installerFixture(t *testing.T, commands string, args ...string) (string, []byte, error) {
	t.Helper()
	directory := t.TempDir()
	for _, name := range []string{"bin/.rook-update", "units", "config", "state", "downloads", "tmp"} {
		if err := os.MkdirAll(filepath.Join(directory, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	source, err := os.ReadFile("../../scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(source)
	script = strings.Replace(script, `INSTALL_DIR="/usr/local/bin"`, `INSTALL_DIR="${ROOK_TEST_ROOT}/bin"`, 1)
	script = strings.Replace(script, `CONFIG_DIR="/etc/rook"`, `CONFIG_DIR="${ROOK_TEST_ROOT}/config"`, 1)
	script = strings.Replace(script, `STATE_DIR="/var/lib/rook"`, `STATE_DIR="${ROOK_TEST_ROOT}/state"`, 1)
	script = strings.ReplaceAll(script, "/etc/systemd/system/", "${ROOK_TEST_ROOT}/units/")
	script = strings.TrimSuffix(script, "main \"$@\"\n")
	script += `
systemctl() { printf '%s\n' "$*" >> "$ROOK_TEST_ROOT/systemctl.log"; }
detect_os() { echo linux; }
detect_arch() { echo amd64; }
download() { cp "$ROOK_TEST_ROOT/downloads/${1##*/}" "$2"; }
` + commands + "\n"
	path := filepath.Join(directory, "fixture.sh")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", append([]string{path}, args...)...)
	command.Env = append(os.Environ(), "ROOK_TEST_ROOT="+directory, "TMPDIR="+filepath.Join(directory, "tmp"))
	output, err := command.CombinedOutput()
	return directory, output, err
}

func TestInstallerFlagControlsDaemonAndUninstallRemovesUpdater(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			args := []string{}
			if enabled {
				args = append(args, "--auto-update")
			}
			directory, output, err := installerFixture(t, `
AUTO_UPDATE_SUPPORTED=true
write_systemd_unit
start_service
cp "$ROOK_TEST_ROOT/units/rook.service" "$ROOK_TEST_ROOT/daemon-unit"
cp "$ROOK_TEST_ROOT/units/rook-update.service" "$ROOK_TEST_ROOT/helper-unit"
uninstall
`, args...)
			if err != nil {
				t.Fatalf("installer: %v\n%s", err, output)
			}
			unit, err := os.ReadFile(filepath.Join(directory, "daemon-unit"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(unit), "--auto-update") != enabled {
				t.Fatalf("wrong daemon opt-in: %s", unit)
			}
			if !strings.Contains(string(unit), "RuntimeDirectoryMode=0700") {
				t.Fatal("missing private runtime directory")
			}
			helper, _ := os.ReadFile(filepath.Join(directory, "helper-unit"))
			if !strings.Contains(string(helper), "User=root") || !strings.Contains(string(helper), "--apply-auto-update") {
				t.Fatalf("invalid helper: %s", helper)
			}
			log, _ := os.ReadFile(filepath.Join(directory, "systemctl.log"))
			for _, expected := range []string{"enable --now rook-update.timer", "stop rook-update.timer rook-update.service", "disable rook-update.timer"} {
				if !strings.Contains(string(log), expected) {
					t.Fatalf("missing %q in %s", expected, log)
				}
			}
			for _, name := range []string{"rook.service", "rook-update.service", "rook-update.timer"} {
				if _, err := os.Stat(filepath.Join(directory, "units", name)); !os.IsNotExist(err) {
					t.Fatalf("unit %s retained: %v", name, err)
				}
			}
		})
	}
}

func TestInstallerRejectsOptInForOldBinaryAndClearsPendingManualUpdate(t *testing.T) {
	for _, supported := range []bool{false, true} {
		t.Run(fmt.Sprint(supported), func(t *testing.T) {
			help := "  -version"
			if supported {
				help = "  -auto-update"
			}
			binary := []byte("#!/bin/sh\nprintf '%s\\n' '" + help + "'\n")
			hash := sha256.Sum256(binary)
			commands := fmt.Sprintf(`
cat > "$ROOK_TEST_ROOT/downloads/rook-linux-amd64" <<'BINARY'
%sBINARY
printf '%%s\n' '%x  rook-linux-amd64' > "$ROOK_TEST_ROOT/downloads/rook-linux-amd64.sha256"
printf 'old binary' > "$ROOK_TEST_ROOT/bin/rook"
printf 'pending' > "$ROOK_TEST_ROOT/bin/.rook-update/pending.json"
download_binary
`, binary, hash)
			directory, output, err := installerFixture(t, commands, "--auto-update")
			actual, readErr := os.ReadFile(filepath.Join(directory, "bin", "rook"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if supported {
				if err != nil || string(actual) != string(binary) {
					t.Fatalf("supported install: %v, %q\n%s", err, actual, output)
				}
				if _, err := os.Stat(filepath.Join(directory, "bin/.rook-update/pending.json")); !os.IsNotExist(err) {
					t.Fatal("manual install did not clear pending update")
				}
			} else if err == nil || string(actual) != "old binary" || !strings.Contains(string(output), "does not support --auto-update") {
				t.Fatalf("unsupported install: %v, %q\n%s", err, actual, output)
			}
		})
	}
}
