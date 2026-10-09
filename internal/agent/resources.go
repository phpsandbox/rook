package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
)

var resourceKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

var serviceIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func resourceProject(key string) string {
	if serviceIDPattern.MatchString(key) {
		return "rook-service-" + key
	}
	return "rook-resources-" + key
}

func databaseServiceName(key string) string {
	if serviceIDPattern.MatchString(key) {
		return "mysql"
	}
	return "database"
}

type ResourceRuntime struct {
	Network string
	Mounts  map[string]string
	Env     map[string]string
}

type resourceCredentials struct {
	DatabasePassword string `json:"databasePassword"`
	RootPassword     string `json:"rootPassword"`
	CachePassword    string `json:"cachePassword"`
}

type composeDocument struct {
	Services map[string]composeService `json:"services"`
	Networks map[string]composeNetwork `json:"networks"`
	Volumes  map[string]composeVolume  `json:"volumes"`
}

type composeService struct {
	Image       string             `json:"image"`
	Restart     string             `json:"restart"`
	Environment map[string]string  `json:"environment,omitempty"`
	Command     []string           `json:"command,omitempty"`
	Volumes     []string           `json:"volumes"`
	Networks    []string           `json:"networks"`
	Healthcheck composeHealthcheck `json:"healthcheck"`
}

type composeNetwork struct {
	Name string `json:"name"`
}
type composeVolume struct {
	Name string `json:"name"`
}
type composeHealthcheck struct {
	Test     []string `json:"test"`
	Interval string   `json:"interval"`
	Timeout  string   `json:"timeout"`
	Retries  int      `json:"retries"`
}

func validateResources(plan Plan) error {
	managed := false
	for kind, selection := range plan.Resources {
		switch kind {
		case "database", "cache", "storage", "worker", "scheduler":
		default:
			return fmt.Errorf("unsupported SSH resource %q", kind)
		}
		switch selection.Mode {
		case "none", "external":
		case "reuse":
			managed = true
			if kind != "database" || !resourceKeyPattern.MatchString(plan.DatabaseServiceKey) || (selection.Type != "" && selection.Type != "mysql") || (selection.Version != "" && selection.Version != "8.4") {
				return fmt.Errorf("database reuse requires a valid service key")
			}
		case "create":
			managed = true
			switch kind {
			case "database":
				if (selection.Type != "" && selection.Type != "mysql") || (selection.Version != "" && selection.Version != "8.4") {
					return fmt.Errorf("SSH database requires MySQL 8.4")
				}
			case "cache":
				if (selection.Type != "" && selection.Type != "redis") || (selection.Version != "" && selection.Version != "7") {
					return fmt.Errorf("SSH cache requires Redis 7")
				}
			case "storage":
				if (selection.Type != "" && selection.Type != "local") || selection.Version != "" {
					return fmt.Errorf("SSH storage requires local persistent files")
				}
			default:
				return fmt.Errorf("SSH cannot create %s resources", kind)
			}
		default:
			return fmt.Errorf("unsupported SSH resource mode %q", selection.Mode)
		}
	}
	if managed && !resourceKeyPattern.MatchString(plan.ResourceKey) {
		return fmt.Errorf("managed resources require a valid project resource key")
	}
	return nil
}

func hasManagedResource(plan Plan, kind string) bool {
	return plan.Resources[kind].Mode == "create" || (kind == "database" && plan.Resources[kind].Mode == "reuse" && plan.DatabaseServiceKey == plan.ResourceKey)
}

func (d *DockerManager) PrepareResources(ctx context.Context, plan Plan, stateDir string) (ResourceRuntime, error) {
	d.resourcesMu.Lock()
	defer d.resourcesMu.Unlock()
	runtime := ResourceRuntime{Env: map[string]string{}, Mounts: map[string]string{}}
	if err := validateResources(plan); err != nil {
		return runtime, err
	}
	if !hasManagedResource(plan, "database") && !hasManagedResource(plan, "cache") && !hasManagedResource(plan, "storage") && plan.Resources["database"].Mode != "reuse" {
		return runtime, nil
	}
	project := resourceProject(plan.ResourceKey)
	dir := filepath.Join(stateDir, "resources", plan.ResourceKey)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return runtime, fmt.Errorf("create resource directory: %w", err)
	}
	if plan.Resources["database"].Mode == "create" {
		if _, err := os.Stat(filepath.Join(dir, "database-allocation.json")); err == nil {
			return runtime, fmt.Errorf("remove this app's existing database before creating a different service")
		} else if !os.IsNotExist(err) {
			return runtime, err
		}
	}
	credentials, err := loadResourceCredentials(dir)
	if err != nil {
		return runtime, err
	}
	if plan.Resources["database"].Mode == "reuse" {
		if err := d.requireDatabaseService(ctx, plan.DatabaseServiceKey, stateDir); err != nil {
			return runtime, err
		}
	}
	doc, runtime := resourceCompose(plan, credentials)
	content, err := json.Marshal(doc)
	if err != nil {
		return runtime, fmt.Errorf("encode resources: %w", err)
	}
	path := filepath.Join(dir, "compose.json")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return runtime, fmt.Errorf("save resource plan: %w", err)
	}
	// Storage-only plans still need a network and volume, which Compose does not create without a service.
	if len(doc.Services) > 0 {
		if err := d.compose(ctx, project, path, "up", "-d", "--wait", "--wait-timeout", "120"); err != nil {
			return runtime, err
		}
	} else {
		if _, err := exec.CommandContext(ctx, d.bin, "network", "inspect", runtime.Network).Output(); err != nil {
			if err := exec.CommandContext(ctx, d.bin, "network", "create", runtime.Network).Run(); err != nil {
				return runtime, fmt.Errorf("create resource network: %w", err)
			}
		}
	}
	for name := range runtime.Mounts {
		if err := exec.CommandContext(ctx, d.bin, "volume", "create", name).Run(); err != nil {
			return runtime, fmt.Errorf("create file volume: %w", err)
		}
	}
	if plan.Resources["database"].Mode == "reuse" && plan.DatabaseServiceKey != plan.ResourceKey {
		if err := d.prepareDatabaseAllocation(ctx, plan, credentials, stateDir, &runtime); err != nil {
			return runtime, err
		}
	}
	return runtime, nil
}

func (d *DockerManager) compose(ctx context.Context, project, path string, args ...string) error {
	commandArgs := append([]string{"compose", "--project-name", project, "--file", path}, args...)
	if err := exec.CommandContext(ctx, d.bin, commandArgs...).Run(); err != nil {
		return fmt.Errorf("resource Compose operation failed: %w", err)
	}
	return nil
}

func loadResourceCredentials(dir string) (resourceCredentials, error) {
	path := filepath.Join(dir, "credentials.json")
	var credentials resourceCredentials
	content, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(content, &credentials); err != nil {
			return credentials, fmt.Errorf("read resource credentials: %w", err)
		}
		if credentials.DatabasePassword == "" || credentials.RootPassword == "" || credentials.CachePassword == "" {
			return credentials, fmt.Errorf("resource credentials are incomplete")
		}
		return credentials, nil
	}
	if !os.IsNotExist(err) {
		return credentials, fmt.Errorf("read resource credentials: %w", err)
	}
	passwords := []*string{&credentials.DatabasePassword, &credentials.RootPassword, &credentials.CachePassword}
	for _, password := range passwords {
		value := make([]byte, 32)
		if _, err := rand.Read(value); err != nil {
			return credentials, fmt.Errorf("generate resource credentials: %w", err)
		}
		*password = hex.EncodeToString(value)
	}
	content, err = json.Marshal(credentials)
	if err != nil {
		return credentials, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return credentials, fmt.Errorf("create resource credentials: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(content); err != nil {
		return credentials, fmt.Errorf("save resource credentials: %w", err)
	}
	return credentials, nil
}

func resourceCompose(plan Plan, credentials resourceCredentials) (composeDocument, ResourceRuntime) {
	project := resourceProject(plan.ResourceKey)
	doc := composeDocument{Services: map[string]composeService{}, Networks: map[string]composeNetwork{"resources": {Name: project}}, Volumes: map[string]composeVolume{}}
	runtime := ResourceRuntime{Network: project, Env: map[string]string{}, Mounts: map[string]string{}}
	if hasManagedResource(plan, "database") {
		doc.Volumes["database"] = composeVolume{Name: project + "-database"}
		doc.Services[databaseServiceName(plan.ResourceKey)] = composeService{
			Image: "mysql:8.4", Restart: "unless-stopped", Networks: []string{"resources"}, Volumes: []string{"database:/var/lib/mysql"},
			Environment: map[string]string{"MYSQL_DATABASE": "app", "MYSQL_USER": "app", "MYSQL_PASSWORD": credentials.DatabasePassword, "MYSQL_ROOT_PASSWORD": credentials.RootPassword},
			Healthcheck: composeHealthcheck{Test: []string{"CMD-SHELL", `MYSQL_PWD="$$MYSQL_PASSWORD" mysql -h 127.0.0.1 -u app app -e 'SELECT 1'`}, Interval: "2s", Timeout: "5s", Retries: 60},
		}
		runtime.Env = map[string]string{"DB_CONNECTION": "mysql", "DB_HOST": databaseServiceName(plan.ResourceKey), "DB_PORT": "3306", "DB_DATABASE": "app", "DB_USERNAME": "app", "DB_PASSWORD": credentials.DatabasePassword, "DB_URL": ""}
	}
	if hasManagedResource(plan, "cache") {
		doc.Volumes["cache"] = composeVolume{Name: project + "-cache"}
		doc.Services["cache"] = composeService{
			Image: "redis:7-alpine", Restart: "unless-stopped", Networks: []string{"resources"}, Volumes: []string{"cache:/data"},
			Command:     []string{"redis-server", "--appendonly", "yes", "--requirepass", credentials.CachePassword},
			Environment: map[string]string{"REDISCLI_AUTH": credentials.CachePassword},
			Healthcheck: composeHealthcheck{Test: []string{"CMD", "redis-cli", "ping"}, Interval: "2s", Timeout: "5s", Retries: 30},
		}
		runtime.Env["REDIS_HOST"] = "cache"
		runtime.Env["REDIS_PORT"] = "6379"
		runtime.Env["REDIS_PASSWORD"] = credentials.CachePassword
		runtime.Env["REDIS_URL"] = ""
		runtime.Env["REDIS_CLIENT"] = "phpredis"
		runtime.Env["CACHE_STORE"] = "redis"
		runtime.Env["SESSION_DRIVER"] = "redis"
	}
	if hasManagedResource(plan, "storage") {
		runtime.Mounts[project+"-files"] = "/app/storage/app"
	}
	return doc, runtime
}

func mergeResourceEnvironment(production, resources map[string]string) map[string]string {
	result := make(map[string]string, len(production)+len(resources))
	for key, value := range production {
		result[key] = value
	}
	for key, value := range resources {
		result[key] = value
	}
	return result
}

func (d *DockerManager) DeleteResources(ctx context.Context, key, stateDir string) error {
	if key == "" {
		return nil
	}
	if !resourceKeyPattern.MatchString(key) {
		return fmt.Errorf("invalid resource key")
	}
	d.resourcesMu.Lock()
	defer d.resourcesMu.Unlock()
	if err := d.deleteDatabaseAllocation(ctx, key, stateDir); err != nil {
		return err
	}
	project := resourceProject(key)
	dir := filepath.Join(stateDir, "resources", key)
	path := filepath.Join(dir, "compose.json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	var doc composeDocument
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		return fmt.Errorf("read retained resource plan: %w", err)
	}
	if len(doc.Services) > 0 {
		if err := d.compose(ctx, project, path, "down", "--volumes", "--remove-orphans"); err != nil {
			return err
		}
	}
	for _, suffix := range []string{"database", "cache", "files"} {
		volume := project + "-" + suffix
		if err := exec.CommandContext(ctx, d.bin, "volume", "inspect", volume).Run(); err == nil {
			if err := exec.CommandContext(ctx, d.bin, "volume", "rm", volume).Run(); err != nil {
				return fmt.Errorf("remove resource volume: %w", err)
			}
		}
	}
	if err := exec.CommandContext(ctx, d.bin, "network", "inspect", project).Run(); err == nil {
		if err := exec.CommandContext(ctx, d.bin, "network", "rm", project).Run(); err != nil {
			return fmt.Errorf("remove resource network: %w", err)
		}
	}
	return os.RemoveAll(dir)
}
