package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareSourceChecksOutPinnedCommitInsteadOfBranchHead(t *testing.T) {
	repo := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=commit.gpgsign", "GIT_CONFIG_VALUE_0=false")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git failed: %v: %s", err, output)
		}
		return strings.TrimSpace(string(output))
	}
	runGit("init", "-b", "main")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@example.test")
	path := filepath.Join(repo, "version.txt")
	if err := os.WriteFile(path, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "first")
	revision := runGit("rev-parse", "HEAD")
	if err := os.WriteFile(path, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit("commit", "-am", "second")
	for _, scenario := range []struct{ name, ref, content string }{
		{"pinned", revision, "first"}, {"branch", "main", "second"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "source")
			if err := PrepareSource(context.Background(), SourceRef{GitURL: repo, Ref: scenario.ref}, dest); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filepath.Join(dest, "version.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(content) != scenario.content {
				t.Fatalf("source content = %q, want %q", content, scenario.content)
			}
		})
	}
	dest := filepath.Join(t.TempDir(), "missing")
	if err := PrepareSource(context.Background(), SourceRef{GitURL: repo, Ref: strings.Repeat("a", 40)}, dest); err == nil {
		t.Fatal("unavailable revision must fail instead of publishing branch head")
	}
}
