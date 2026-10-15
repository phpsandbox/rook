package host

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestHostArchiveExtractsVerifiedRuntimeFiles(t *testing.T) {
	workspace := t.TempDir()
	bundle := testArchive(t, map[string]string{
		"Dockerfile": "FROM scratch\n",
		".phpsandbox/runtime/laravel/laravel-start.sh": "#!/bin/sh\n",
		".phpsandbox/runtime/laravel/Caddyfile":        ":8000\n",
	})

	if err := extractTarGzip(workspace, bundle); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(filepath.Join(workspace, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "FROM scratch\n" {
		t.Fatalf("Dockerfile = %q", content)
	}
}

func TestHostArchiveRejectsUnsafePaths(t *testing.T) {
	workspace := t.TempDir()
	bundle := testArchive(t, map[string]string{
		"../escape": "nope",
	})

	if err := extractTarGzip(workspace, bundle); err == nil {
		t.Fatal("expected unsafe path error")
	}
}

func testArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var payload bytes.Buffer
	gzipWriter := gzip.NewWriter(&payload)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, content := range files {
		data := []byte(content)
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(data)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}

	return payload.Bytes()
}

func TestHostArchiveRejectsExistingDirectorySymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "victim"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "link")); err != nil {
		t.Fatal(err)
	}
	if err := extractTarGzip(workspace, testArchive(t, map[string]string{"link/victim": "replacement"})); err == nil {
		t.Fatal("directory symlink escape accepted")
	}
	content, err := os.ReadFile(filepath.Join(outside, "victim"))
	if err != nil || string(content) != "original" {
		t.Fatalf("outside file changed: %q, %v", content, err)
	}
}

func TestHostArchiveReplacesLinksWithoutChangingOutsideFiles(t *testing.T) {
	for _, hardlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "symlink", true: "hardlink"}[hardlink], func(t *testing.T) {
			base := t.TempDir()
			workspace := filepath.Join(base, "workspace")
			outside := filepath.Join(base, "victim")
			if err := os.Mkdir(workspace, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(outside, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(workspace, "file")
			link := os.Symlink
			if hardlink {
				link = os.Link
			}
			if err := link(outside, target); err != nil {
				t.Fatal(err)
			}
			if err := extractTarGzip(workspace, testArchive(t, map[string]string{"file": "replacement"})); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(outside)
			if err != nil || string(content) != "original" {
				t.Fatalf("outside file changed: %q, %v", content, err)
			}
			content, err = os.ReadFile(target)
			if err != nil || string(content) != "replacement" {
				t.Fatalf("extracted file: %q, %v", content, err)
			}
		})
	}
}

func TestHostArchiveCreatesDestinationAndReplacesExistingFiles(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "new", "workspace")
	for _, content := range []string{"first", "second"} {
		if err := extractTarGzip(workspace, testArchive(t, map[string]string{"nested/file": content})); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(workspace, "nested/file"))
		if err != nil || string(got) != content {
			t.Fatalf("content=%q, error=%v", got, err)
		}
	}
}
