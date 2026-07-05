package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func PrepareSource(ctx context.Context, source SourceRef, workspace string) error {
	if strings.TrimSpace(source.Path) != "" {
		return copyDirectory(source.Path, workspace)
	}
	if strings.TrimSpace(source.GitURL) == "" {
		return fmt.Errorf("source requires gitUrl or path")
	}
	authEnv, cleanup, err := gitCredentialEnv(source)
	if err != nil {
		return err
	}
	defer cleanup()

	cmd := exec.CommandContext(ctx, "git", gitCloneArgs(source, workspace)...)
	if len(authEnv) > 0 {
		cmd.Env = append(os.Environ(), authEnv...)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func gitCloneArgs(source SourceRef, workspace string) []string {
	args := []string{"-c", "credential.helper=", "clone", "--depth=1"}
	if strings.TrimSpace(source.Ref) != "" {
		args = append(args, "--branch", source.Ref)
	}
	return append(args, source.GitURL, workspace)
}

func gitCredentialEnv(source SourceRef) ([]string, func(), error) {
	password := strings.TrimSpace(source.GitPassword)
	if password == "" {
		password = strings.TrimSpace(source.GitToken)
	}
	if password == "" {
		return nil, func() {}, nil
	}
	username := strings.TrimSpace(source.GitUsername)
	if username == "" {
		username = "x-token"
	}

	askpass, err := os.CreateTemp("", "rook-git-askpass-*")
	if err != nil {
		return nil, func() {}, fmt.Errorf("create git askpass: %w", err)
	}
	path := askpass.Name()
	script := `#!/bin/sh
case "$1" in
  *Username*|*username*) printf '%s\n' "$ROOK_GIT_USERNAME" ;;
  *) printf '%s\n' "$ROOK_GIT_PASSWORD" ;;
esac
`
	if _, err := askpass.WriteString(script); err != nil {
		_ = askpass.Close()
		_ = os.Remove(path)
		return nil, func() {}, fmt.Errorf("write git askpass: %w", err)
	}
	if err := askpass.Close(); err != nil {
		_ = os.Remove(path)
		return nil, func() {}, fmt.Errorf("close git askpass: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		_ = os.Remove(path)
		return nil, func() {}, fmt.Errorf("chmod git askpass: %w", err)
	}

	return []string{
			"GIT_ASKPASS=" + path,
			"GIT_TERMINAL_PROMPT=0",
			"ROOK_GIT_USERNAME=" + username,
			"ROOK_GIT_PASSWORD=" + password,
		}, func() {
			_ = os.Remove(path)
		}, nil
}

func copyDirectory(source string, destination string) error {
	source = filepath.Clean(source)
	destination = filepath.Clean(destination)
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		defer output.Close()
		_, err = io.Copy(output, input)
		return err
	})
}
