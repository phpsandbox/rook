package host

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func extractTarGzip(destination string, payload []byte) error {
	reader, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("read host archive gzip: %w", err)
	}
	defer reader.Close()
	if err := os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer root.Close()

	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read host archive tar: %w", err)
		}
		if header == nil {
			continue
		}
		target, err := safeBundleTarget(destination, header.Name)
		if err != nil {
			return err
		}
		// Root resolves every component beneath the open directory, including
		// symlinks swapped concurrently by a process in the workspace.
		name, err := filepath.Rel(filepath.Clean(destination), target)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, os.FileMode(header.Mode).Perm()); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return err
			}
			if err := writeArchiveFile(root, name, tarReader, os.FileMode(header.Mode).Perm()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported host archive entry type %d for %s", header.Typeflag, header.Name)
		}
	}
	return nil
}

func writeArchiveFile(root *os.Root, name string, data io.Reader, mode os.FileMode) error {
	// Replace the directory entry rather than truncating an existing inode.
	// This also protects files outside the workspace reached by hardlinks.
	temporary := filepath.Join(filepath.Dir(name), ".rook-archive-"+rand.Text())
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	if _, err := io.Copy(file, data); err != nil {
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
	return root.Rename(temporary, name)
}

func safeBundleTarget(destination string, name string) (string, error) {
	cleanName := filepath.Clean(name)
	if cleanName == "." || strings.HasPrefix(cleanName, ".."+string(filepath.Separator)) || filepath.IsAbs(cleanName) {
		return "", fmt.Errorf("unsafe host archive path %q", name)
	}
	cleanDestination := filepath.Clean(destination)
	target := filepath.Join(cleanDestination, cleanName)
	if target != cleanDestination && !strings.HasPrefix(target, cleanDestination+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe host archive path %q", name)
	}
	return target, nil
}
