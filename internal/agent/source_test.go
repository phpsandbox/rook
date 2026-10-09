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
	dest := filepath.Join(t.TempDir(), "source")
	if err := PrepareSource(context.Background(), SourceRef{GitURL: repo, Ref: revision}, dest); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dest, "version.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "first" {
		t.Fatalf("published branch head instead of pinned commit: %q", content)
	}
	for _, ref := range []string{"", "main", " " + revision} {
		if err := PrepareSource(context.Background(), SourceRef{GitURL: repo, Ref: ref}, filepath.Join(t.TempDir(), "rejected")); err == nil {
			t.Fatal("unpinned source accepted")
		}
	}
	dest = filepath.Join(t.TempDir(), "missing")
	if err := PrepareSource(context.Background(), SourceRef{GitURL: repo, Ref: strings.Repeat("a", 40)}, dest); err == nil {
		t.Fatal("unavailable revision must fail instead of publishing branch head")
	}
}

func TestGitAskpassPreservesCredentialsExactly(t *testing.T) {
	env, cleanup, err := gitCredentialEnv(SourceRef{GitUsername: " user ", GitPassword: " password "})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	path := strings.TrimPrefix(env[0], "GIT_ASKPASS=")
	for prompt, want := range map[string]string{"Username": " user \n", "Password": " password \n"} {
		cmd := exec.Command(path, prompt)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != want {
			t.Fatal("Git credential was altered")
		}
	}
}
