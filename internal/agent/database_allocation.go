package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type databaseAllocation struct {
	ServiceKey string `json:"serviceKey"`
}

func allocationName(key string) string {
	digest := sha256.Sum256([]byte(key))
	return "app_" + hex.EncodeToString(digest[:8])
}

func databaseContainer(key string) string {
	return resourceProject(key) + "-" + databaseServiceName(key) + "-1"
}

func readDatabaseCredentials(stateDir, key string) (resourceCredentials, error) {
	var credentials resourceCredentials
	content, err := os.ReadFile(filepath.Join(stateDir, "resources", key, "credentials.json"))
	if err != nil {
		return credentials, fmt.Errorf("read database service credentials: %w", err)
	}
	if err := json.Unmarshal(content, &credentials); err != nil {
		return credentials, err
	}
	if credentials.RootPassword == "" {
		return credentials, fmt.Errorf("database service credentials are incomplete")
	}
	return credentials, nil
}

func (d *DockerManager) requireDatabaseService(ctx context.Context, serviceKey, stateDir string) error {
	if _, err := readDatabaseCredentials(stateDir, serviceKey); err != nil {
		return err
	}
	var running bool
	out, err := exec.CommandContext(ctx, d.bin, "inspect", "--format", "{{json .State.Running}}", databaseContainer(serviceKey)).Output()
	if err != nil {
		return fmt.Errorf("selected MySQL service is unavailable: %w", err)
	}
	if err := json.Unmarshal(out, &running); err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("selected MySQL service is not running")
	}
	return nil
}

func (d *DockerManager) databaseSQL(ctx context.Context, serviceKey, password, sql string) error {
	command := exec.CommandContext(ctx, d.bin, "exec", "-i", "-e", "MYSQL_PWD", databaseContainer(serviceKey), "mysql", "-h", "127.0.0.1", "-u", "root")
	command.Env = append(os.Environ(), "MYSQL_PWD="+password)
	command.Stdin = strings.NewReader(sql)
	if err := command.Run(); err != nil {
		return fmt.Errorf("database allocation operation failed: %w", err)
	}
	return nil
}

func (d *DockerManager) prepareDatabaseAllocation(ctx context.Context, plan Plan, credentials resourceCredentials, stateDir string, runtime *ResourceRuntime) error {
	service, err := readDatabaseCredentials(stateDir, plan.DatabaseServiceKey)
	if err != nil {
		return err
	}
	binding := databaseAllocation{ServiceKey: plan.DatabaseServiceKey}
	path := filepath.Join(stateDir, "resources", plan.ResourceKey, "database-allocation.json")
	if previous, err := os.ReadFile(path); err == nil {
		var existing databaseAllocation
		if err := json.Unmarshal(previous, &existing); err != nil {
			return err
		}
		if existing.ServiceKey != binding.ServiceKey {
			return fmt.Errorf("this app already has a database on another service; remove it before switching")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	content, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	// Persist intent before SQL so cleanup can reconcile partially completed allocation.
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return err
	}
	password, err := hex.DecodeString(credentials.DatabasePassword)
	if err != nil || len(password) != 32 {
		return fmt.Errorf("invalid database allocation credentials")
	}
	name := allocationName(plan.ResourceKey)
	sql := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`; CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'; GRANT ALL ON `%s`.* TO '%s'@'%%';", name, name, credentials.DatabasePassword, name, name)
	if err := d.databaseSQL(ctx, plan.DatabaseServiceKey, service.RootPassword, sql); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, d.bin, "inspect", "--format", "{{json .NetworkSettings.Networks}}", databaseContainer(plan.DatabaseServiceKey)).Output()
	if err != nil {
		return err
	}
	var networks map[string]json.RawMessage
	if err := json.Unmarshal(out, &networks); err != nil {
		return err
	}
	if _, connected := networks[runtime.Network]; !connected {
		if err := exec.CommandContext(ctx, d.bin, "network", "connect", "--alias", "database", runtime.Network, databaseContainer(plan.DatabaseServiceKey)).Run(); err != nil {
			return fmt.Errorf("connect shared database: %w", err)
		}
	}
	for key, value := range map[string]string{"DB_CONNECTION": "mysql", "DB_HOST": "database", "DB_PORT": "3306", "DB_DATABASE": name, "DB_USERNAME": name, "DB_PASSWORD": credentials.DatabasePassword, "DB_URL": ""} {
		runtime.Env[key] = value
	}
	return nil
}

func (d *DockerManager) deleteDatabaseAllocation(ctx context.Context, key, stateDir string) error {
	path := filepath.Join(stateDir, "resources", key, "database-allocation.json")
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var binding databaseAllocation
	if err := json.Unmarshal(content, &binding); err != nil {
		return err
	}
	if !resourceKeyPattern.MatchString(binding.ServiceKey) || binding.ServiceKey == key {
		return fmt.Errorf("invalid shared database binding")
	}
	service, err := readDatabaseCredentials(stateDir, binding.ServiceKey)
	if err != nil {
		return err
	}
	name := allocationName(key)
	if err := d.databaseSQL(ctx, binding.ServiceKey, service.RootPassword, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`; DROP USER IF EXISTS '%s'@'%%';", name, name)); err != nil {
		return err
	}
	project := resourceProject(key)
	out, err := exec.CommandContext(ctx, d.bin, "inspect", "--format", "{{json .NetworkSettings.Networks}}", databaseContainer(binding.ServiceKey)).Output()
	if err != nil {
		return err
	}
	var networks map[string]json.RawMessage
	if err := json.Unmarshal(out, &networks); err != nil {
		return err
	}
	if _, connected := networks[project]; connected {
		if err := exec.CommandContext(ctx, d.bin, "network", "disconnect", project, databaseContainer(binding.ServiceKey)).Run(); err != nil {
			return err
		}
	}
	return os.Remove(path)
}
