package agent

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResourceValidationRejectsUnsupportedAndUnownedPlans(t *testing.T) {
	cases := []Plan{
		{ResourceKey: "borrower", DatabaseServiceKey: "../owner", Resources: map[string]ResourceSelection{"database": {Mode: "reuse"}}},
		{ResourceKey: "borrower", DatabaseServiceKey: "owner", Resources: map[string]ResourceSelection{"cache": {Mode: "reuse"}}},
		{ResourceKey: "borrower", DatabaseServiceKey: "owner", Resources: map[string]ResourceSelection{"database": {Mode: "reuse", Type: "postgres"}}},
		{Resources: map[string]ResourceSelection{"database": {Mode: "create", Type: "mysql"}}},
		{ResourceKey: "../other", Resources: map[string]ResourceSelection{"storage": {Mode: "create"}}},
		{ResourceKey: "project", Resources: map[string]ResourceSelection{"database": {Mode: "create", Type: "postgres"}}},
		{ResourceKey: "project", Resources: map[string]ResourceSelection{"cache": {Mode: "create", Version: "latest"}}},
		{ResourceKey: "project", Resources: map[string]ResourceSelection{"worker": {Mode: "create"}}},
	}
	for _, plan := range cases {
		if err := validateResources(plan); err == nil {
			t.Fatal("unsupported plan accepted")
		}
	}
}

func TestResourceCredentialsSurviveAgentRestartAndStayPrivate(t *testing.T) {
	dir := t.TempDir()
	first, err := loadResourceCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadResourceCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("resource credentials changed across releases")
	}
	info, err := os.Stat(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatal("resource credentials are not private")
	}
}

func TestManagedResourcesPersistAcrossComposeRestart(t *testing.T) {
	if os.Getenv("ROOK_DOCKER_TEST") != "1" {
		t.Skip("set ROOK_DOCKER_TEST=1 to test real Docker resources")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	manager := NewDockerManager()
	key := testServiceID(t)
	plan := Plan{ResourceKey: key, Resources: map[string]ResourceSelection{
		"database": {Mode: "create", Type: "mysql", Version: "8.4"},
		"cache":    {Mode: "create", Type: "redis", Version: "7"},
		"storage":  {Mode: "create", Type: "local"},
	}}
	dir := t.TempDir()
	project := resourceProject(key)
	path := filepath.Join(dir, "resources", key, "compose.json")
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = manager.compose(cleanup, project, path, "down", "--volumes")
		_ = exec.CommandContext(cleanup, "docker", "volume", "rm", project+"-files").Run()
	})
	runtime, err := manager.PrepareResources(ctx, plan, dir)
	if err != nil {
		t.Fatal(err)
	}
	container := func(service string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", "compose", "--project-name", project, "--file", path, "ps", "-q", service).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	sql := func(query string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", "exec", "-e", "MYSQL_PWD="+runtime.Env["DB_PASSWORD"], container(databaseServiceName(key)), "mysql", "-u", "app", "-N", "app", "-e", query).Output()
		if err != nil {
			t.Fatal("MySQL query failed", err)
		}
		return strings.TrimSpace(string(out))
	}
	sql("CREATE TABLE proof (value VARCHAR(32)); INSERT INTO proof VALUES ('persistent');")
	cache := func(args ...string) string {
		t.Helper()
		command := append([]string{"exec", container("cache"), "redis-cli"}, args...)
		out, err := exec.CommandContext(ctx, "docker", command...).Output()
		if err != nil {
			t.Fatal("Redis query failed", err)
		}
		return strings.TrimSpace(string(out))
	}
	if cache("SET", "proof", "persistent") != "OK" {
		t.Fatal("Redis write failed")
	}
	if err := manager.compose(ctx, project, path, "down"); err != nil {
		t.Fatal(err)
	}
	next, err := manager.PrepareResources(ctx, plan, dir)
	if err != nil {
		t.Fatal(err)
	}
	if next.Env["DB_PASSWORD"] != runtime.Env["DB_PASSWORD"] {
		t.Fatal("database credentials changed")
	}
	if sql("SELECT value FROM proof") != "persistent" {
		t.Fatal("MySQL data lost across restart")
	}
	if cache("GET", "proof") != "persistent" {
		t.Fatal("Redis data lost across restart")
	}
	volume := project + "-files"
	if err := exec.CommandContext(ctx, "docker", "run", "--rm", "-v", volume+":/files", "redis:7-alpine", "sh", "-c", "echo persistent > /files/proof").Run(); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "-v", volume+":/files", "redis:7-alpine", "cat", "/files/proof").Output()
	if err != nil || strings.TrimSpace(string(out)) != "persistent" {
		t.Fatal("persistent file storage failed", err)
	}
	if err := manager.DeleteResources(ctx, key, dir); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"database", "cache", "files"} {
		if err := exec.CommandContext(ctx, "docker", "volume", "inspect", project+"-"+suffix).Run(); err == nil {
			t.Fatal("explicit resource deletion retained a volume")
		}
	}

}

func TestSharedMySQLKeepsAppDatabasesIsolatedAndDeletesOnlyBorrowerData(t *testing.T) {
	if os.Getenv("ROOK_DOCKER_TEST") != "1" {
		t.Skip("set ROOK_DOCKER_TEST=1 to test real Docker resources")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	manager := NewDockerManager()
	key := testServiceID(t)
	borrowerKey := testServiceID(t)
	dir := t.TempDir()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = manager.DeleteResources(cleanup, borrowerKey, dir)
		_ = manager.DeleteResources(cleanup, key, dir)
	})
	owner, err := manager.PrepareResources(ctx, Plan{ResourceKey: key, Resources: map[string]ResourceSelection{"database": {Mode: "create", Type: "mysql", Version: "8.4"}}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	borrowerPlan := Plan{ResourceKey: borrowerKey, DatabaseServiceKey: key, Resources: map[string]ResourceSelection{"database": {Mode: "reuse", Type: "mysql", Version: "8.4"}}}
	borrower, err := manager.PrepareResources(ctx, borrowerPlan, dir)
	if err != nil {
		t.Fatal(err)
	}
	query := func(runtime ResourceRuntime, sql string) ([]byte, error) {
		command := exec.CommandContext(ctx, "docker", "exec", "-e", "MYSQL_PWD", databaseContainer(key), "mysql", "-h", "127.0.0.1", "-u", runtime.Env["DB_USERNAME"], "-N", runtime.Env["DB_DATABASE"], "-e", sql)
		command.Env = append(os.Environ(), "MYSQL_PWD="+runtime.Env["DB_PASSWORD"])
		return command.CombinedOutput()
	}
	if borrower.Env["DB_DATABASE"] == owner.Env["DB_DATABASE"] || borrower.Env["DB_PASSWORD"] == owner.Env["DB_PASSWORD"] {
		t.Fatal("app credentials or databases are shared")
	}
	for _, runtime := range []ResourceRuntime{owner, borrower} {
		if _, err := query(runtime, "CREATE TABLE proof (value VARCHAR(32)); INSERT INTO proof VALUES ('persistent');"); err != nil {
			t.Fatal("database write failed", err)
		}
	}
	if _, err := query(borrower, "SELECT * FROM app.proof"); err == nil {
		t.Fatal("borrower can read owner's database")
	}
	if _, err := query(owner, "SELECT * FROM `"+borrower.Env["DB_DATABASE"]+"`.proof"); err == nil {
		t.Fatal("owner app login can read borrower's database")
	}
	next, err := manager.PrepareResources(ctx, borrowerPlan, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.PrepareResources(ctx, Plan{ResourceKey: borrowerKey, Resources: map[string]ResourceSelection{"database": {Mode: "create", Type: "mysql"}}}, dir); err == nil {
		t.Fatal("creating another service abandoned retained borrower data")
	}
	if next.Env["DB_PASSWORD"] != borrower.Env["DB_PASSWORD"] {
		t.Fatal("borrower credentials changed on republish")
	}
	if out, err := query(next, "SELECT value FROM proof"); err != nil || strings.TrimSpace(string(out)) != "persistent" {
		t.Fatal("borrower data lost on republish", err)
	}
	if err := manager.DeleteResources(ctx, borrowerKey, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := query(borrower, "SELECT value FROM proof"); err == nil {
		t.Fatal("borrower database retained after explicit deletion")
	}
	if out, err := query(owner, "SELECT value FROM proof"); err != nil || strings.TrimSpace(string(out)) != "persistent" {
		t.Fatal("owner data or service damaged by borrower cleanup", err)
	}
}

func testServiceID(t *testing.T) string {
	t.Helper()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
}

func TestServiceNamesPreserveLegacyResourceLocations(t *testing.T) {
	for _, key := range []string{"wenders", "bde017fb-cc01-46c9-9c83-4558dd9f9568"} {
		plan := Plan{ResourceKey: key, Resources: map[string]ResourceSelection{"database": {Mode: "create"}, "cache": {Mode: "create"}, "storage": {Mode: "create"}}}
		doc, runtime := resourceCompose(plan, resourceCredentials{})
		project := "rook-resources-wenders"
		service := "database"
		if key != "wenders" {
			project = "rook-service-" + key
			service = "mysql"
		}
		if runtime.Network != project || databaseContainer(key) != project+"-"+service+"-1" || runtime.Env["DB_HOST"] != service {
			t.Fatal("resource addressing does not match its identity")
		}
		if doc.Volumes["database"].Name != project+"-database" || doc.Volumes["cache"].Name != project+"-cache" || runtime.Mounts[project+"-files"] != "/app/storage/app" {
			t.Fatal("resource volume names do not match their identity")
		}
		if doc.Services[service].Image != "mysql:8.4" {
			t.Fatal("database service is missing")
		}
	}
}
