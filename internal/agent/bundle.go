package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const MaxDeployBundleSize = 5 * 1024 * 1024

func ApplyDeployBundle(workspace string, bundle DeployBundle) error {
	if bundle.Format != "tar.gz" {
		return fmt.Errorf("unsupported deploy bundle format %q", bundle.Format)
	}
	if bundle.Size < 0 {
		return fmt.Errorf("invalid deploy bundle size %d", bundle.Size)
	}
	if bundle.Size > MaxDeployBundleSize {
		return fmt.Errorf("deploy bundle size %d exceeds limit %d", bundle.Size, MaxDeployBundleSize)
	}
	if int64(len(bundle.Data)) != bundle.Size {
		return fmt.Errorf("deploy bundle size mismatch: got %d bytes, expected %d", len(bundle.Data), bundle.Size)
	}
	if int64(len(bundle.Data)) > MaxDeployBundleSize {
		return fmt.Errorf("deploy bundle payload exceeds limit %d", MaxDeployBundleSize)
	}
	if err := verifyDeployBundleChecksum(bundle); err != nil {
		return err
	}
	return extractTarGzip(workspace, bundle.Data)
}

func verifyDeployBundleChecksum(bundle DeployBundle) error {
	expected := strings.TrimSpace(bundle.SHA256)
	if expected == "" {
		return fmt.Errorf("deploy bundle checksum is required")
	}
	expected = strings.TrimPrefix(expected, "sha256:")
	sum := sha256.Sum256(bundle.Data)
	actual := hex.EncodeToString(sum[:])
	if !strings.EqualFold(expected, actual) {
		return fmt.Errorf("deploy bundle checksum mismatch")
	}
	return nil
}

func extractTarGzip(destination string, payload []byte) error {
	reader, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("read deploy bundle gzip: %w", err)
	}
	defer reader.Close()

	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read deploy bundle tar: %w", err)
		}
		if header == nil {
			continue
		}
		target, err := safeBundleTarget(destination, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(file, tarReader); err != nil {
				_ = file.Close()
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported deploy bundle entry type %d for %s", header.Typeflag, header.Name)
		}
	}
	return nil
}

func safeBundleTarget(destination string, name string) (string, error) {
	cleanName := filepath.Clean(name)
	if cleanName == "." || strings.HasPrefix(cleanName, ".."+string(filepath.Separator)) || filepath.IsAbs(cleanName) {
		return "", fmt.Errorf("unsafe deploy bundle path %q", name)
	}
	cleanDestination := filepath.Clean(destination)
	target := filepath.Join(cleanDestination, cleanName)
	if target != cleanDestination && !strings.HasPrefix(target, cleanDestination+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe deploy bundle path %q", name)
	}
	return target, nil
}
