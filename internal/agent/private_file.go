package agent

import (
	"os"
	"path/filepath"
)

func writePrivateFile(path string, content []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".resource-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(content); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}
