package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyDeployBundleExtractsVerifiedRuntimeFiles(t *testing.T) {
	workspace := t.TempDir()
	bundle := testDeployBundle(t, map[string]string{
		"Dockerfile": "FROM scratch\n",
		".phpsandbox/runtime/laravel/laravel-start.sh": "#!/bin/sh\n",
		".phpsandbox/runtime/laravel/Caddyfile":        ":8000\n",
	})

	if err := ApplyDeployBundle(workspace, bundle); err != nil {
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

func TestApplyDeployBundleRejectsUnsafePaths(t *testing.T) {
	workspace := t.TempDir()
	bundle := testDeployBundle(t, map[string]string{
		"../escape": "nope",
	})

	if err := ApplyDeployBundle(workspace, bundle); err == nil {
		t.Fatal("expected unsafe path error")
	}
}

func TestApplyDeployBundleRejectsChecksumMismatch(t *testing.T) {
	workspace := t.TempDir()
	bundle := testDeployBundle(t, map[string]string{
		"Dockerfile": "FROM scratch\n",
	})
	bundle.SHA256 = "sha256:deadbeef"

	if err := ApplyDeployBundle(workspace, bundle); err == nil {
		t.Fatal("expected checksum mismatch")
	}
}

func testDeployBundle(t *testing.T, files map[string]string) DeployBundle {
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

	data := payload.Bytes()
	sum := sha256.Sum256(data)
	return DeployBundle{
		Format: "tar.gz",
		Size:   int64(len(data)),
		SHA256: "sha256:" + hex.EncodeToString(sum[:]),
		Data:   data,
	}
}
