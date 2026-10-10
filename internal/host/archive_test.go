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
