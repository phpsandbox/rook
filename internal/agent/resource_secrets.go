package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var secretNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9]{0,63}$`)
var secretReferencePattern = regexp.MustCompile(`\{\{secret:([a-z0-9-]+):([a-zA-Z0-9]+)\}\}`)

func loadSecrets(dir string, names []string) (map[string]string, error) {
	path := filepath.Join(dir, "credentials.json")
	secrets := map[string]string{}
	content, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(content, &secrets); err != nil {
			return nil, err
		}
		if secrets == nil {
			return nil, fmt.Errorf("invalid local secret store")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	changed := false
	for _, name := range names {
		if secrets[name] != "" {
			continue
		}
		value := make([]byte, 32)
		if _, err := rand.Read(value); err != nil {
			return nil, err
		}
		secrets[name] = hex.EncodeToString(value)
		changed = true
	}
	if changed {
		content, err := json.Marshal(secrets)
		if err != nil {
			return nil, err
		}
		// Atomic replacement keeps the previous credential file intact after an interrupted write.
		temp, err := os.CreateTemp(dir, "credentials-*")
		if err != nil {
			return nil, err
		}
		defer os.Remove(temp.Name())
		if _, err := temp.Write(content); err != nil {
			temp.Close()
			return nil, err
		}
		if err := temp.Close(); err != nil {
			return nil, err
		}
		if err := os.Rename(temp.Name(), path); err != nil {
			return nil, err
		}
	}
	return secrets, nil
}

func resolveExecution(plan ResourceExecution, stateDir string) (ResourceExecution, error) {
	content, err := json.Marshal(plan)
	if err != nil {
		return plan, err
	}
	var resolveError error
	content = secretReferencePattern.ReplaceAllFunc(content, func(reference []byte) []byte {
		parts := secretReferencePattern.FindSubmatch(reference)
		scope, name := string(parts[1]), string(parts[2])
		if scope != plan.Key {
			if _, ok := plan.Dependencies[scope]; !ok {
				resolveError = fmt.Errorf("secret references an undeclared resource")
				return reference
			}
		}
		secrets, err := loadSecrets(filepath.Join(stateDir, "resources", scope), nil)
		if err != nil {
			resolveError = err
			return reference
		}
		value, ok := secrets[name]
		if !ok || value == "" {
			resolveError = fmt.Errorf("required local secret is unavailable")
			return reference
		}
		encoded, _ := json.Marshal(value)
		return encoded[1 : len(encoded)-1]
	})
	if resolveError != nil {
		return plan, resolveError
	}
	var resolved ResourceExecution
	if err := json.Unmarshal(content, &resolved); err != nil {
		return plan, err
	}
	return resolved, nil
}
