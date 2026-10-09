package host

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
		if err := writeFile(path, content, 0600); err != nil {
			return nil, err
		}
	}
	return secrets, nil
}

func (e *Executor) ensureSecrets(scope string, names []string) error {
	if err := validateScope(scope); err != nil {
		return err
	}
	dir := filepath.Join(e.Directory, "secrets", scope)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	_, err := loadSecrets(dir, names)
	return err
}
func validateScope(scope string) error {
	if scope == "." || !filepath.IsLocal(scope) || filepath.Base(scope) != scope {
		return fmt.Errorf("secret scope escapes local directory")
	}
	return nil
}
func (e *Executor) resolve(request Request) (Request, error) {
	data := request.Data
	request.Data = nil
	content, err := json.Marshal(request)
	if err != nil {
		return request, err
	}
	var resolveError error
	content = secretReferencePattern.ReplaceAllFunc(content, func(reference []byte) []byte {
		parts := secretReferencePattern.FindSubmatch(reference)
		scope, name := string(parts[1]), string(parts[2])
		if err := validateScope(scope); err != nil {
			resolveError = err
			return reference
		}
		secrets, err := loadSecrets(filepath.Join(e.Directory, "secrets", scope), nil)
		if err != nil {
			resolveError = err
			return reference
		}
		value, ok := secrets[name]
		if !ok {
			resolveError = fmt.Errorf("local secret is unavailable")
			return reference
		}
		encoded, _ := json.Marshal(value)
		return encoded[1 : len(encoded)-1]
	})
	if resolveError != nil {
		return request, resolveError
	}
	var resolved Request
	err = json.Unmarshal(content, &resolved)
	if err != nil {
		return request, err
	}
	if data != nil {
		value, err := e.resolve(Request{Input: string(data)})
		if err != nil {
			return request, err
		}
		resolved.Data = []byte(value.Input)
	}
	return resolved, nil
}
