package agent

import (
	"fmt"
	"path/filepath"
)

func validateResourceKey(key string) error {
	if key == "." || !filepath.IsLocal(key) || filepath.Base(key) != key {
		return fmt.Errorf("resource key escapes local state directory")
	}
	return nil
}

func validateResourcePaths(plan *ResourceExecution) error {
	if plan == nil {
		return nil
	}
	if err := validateResourceKey(plan.Key); err != nil {
		return err
	}
	for key := range plan.Dependencies {
		if err := validateResourceKey(key); err != nil {
			return err
		}
	}
	return nil
}
