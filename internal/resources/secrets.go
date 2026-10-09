package resources

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

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
		if err := writePrivateFile(path, content); err != nil {
			return nil, err
		}
	}
	return secrets, nil
}

func resolveExecution(plan Plan, stateDir string) (Plan, error) {
	content, err := json.Marshal(plan)
	if err != nil {
		return plan, err
	}
	var resolveError error
	content = secretReferencePattern.ReplaceAllFunc(content, func(reference []byte) []byte {
		parts := secretReferencePattern.FindSubmatch(reference)
		scope, name := string(parts[1]), string(parts[2])
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
	var resolved Plan
	if err := json.Unmarshal(content, &resolved); err != nil {
		return plan, err
	}
	return resolved, nil
}
