package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

func TestExecutionMessagePackPreservesResolvedCompose(t *testing.T) {
	plan := testExecution(t, "services", "project", "borrower")
	content, err := msgpack.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Plan
	if err := msgpack.Unmarshal(content, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Execution, decoded.Execution) {
		t.Fatal("MessagePack changed the resource execution plan")
	}
	if err := validateResourceExecution(decoded.Execution); err != nil {
		t.Fatal(err)
	}
}

func TestExecutionRejectsEscapingScopes(t *testing.T) {
	for _, change := range []func(*ResourceExecution){
		func(p *ResourceExecution) { p.Key = "../other" },
		func(p *ResourceExecution) { p.Runtime.Network = "someone-else" },
		func(p *ResourceExecution) { p.Runtime.Mounts = map[string]string{"other-volume": "/files"} },
		func(p *ResourceExecution) {
			p.Prepare = []ResourceOperation{{Action: "exec", Scope: "undeclared", Container: "rook-service-foreign-mysql-1", Command: []string{"sh"}}}
		},
	} {
		plan := testExecution(t, "services", "project", "borrower")
		change(plan.Execution)
		if err := validateResourceExecution(plan.Execution); err == nil {
			t.Fatal("escaping execution plan accepted")
		}
	}
}

func TestResourceCredentialsSurviveAgentRestartAndStayPrivate(t *testing.T) {
	dir := t.TempDir()
	first, err := loadSecrets(dir, []string{"password"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadSecrets(dir, []string{"password"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
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
	plan := testExecution(t, "services", key, "")
	dir := t.TempDir()
	project := plan.Execution.Project
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
		out, err := exec.CommandContext(ctx, "docker", "exec", "-e", "MYSQL_PWD="+runtime.Env["DB_PASSWORD"], container("mysql"), "mysql", "-u", "app", "-N", "app", "-e", query).Output()
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
	ownerPlan := testExecution(t, "services", key, "")
	owner, err := manager.PrepareResources(ctx, ownerPlan, dir)
	if err != nil {
		t.Fatal(err)
	}
	borrowerPlan := testExecution(t, "allocation", key, borrowerKey)
	borrower, err := manager.PrepareResources(ctx, borrowerPlan, dir)
	if err != nil {
		t.Fatal(err)
	}
	query := func(runtime ResourceRuntime, sql string) ([]byte, error) {
		command := exec.CommandContext(ctx, "docker", "exec", "-e", "MYSQL_PWD", ownerPlan.Execution.Project+"-mysql-1", "mysql", "-h", "127.0.0.1", "-u", runtime.Env["DB_USERNAME"], "-N", runtime.Env["DB_DATABASE"], "-e", sql)
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
	if _, err := manager.PrepareResources(ctx, testExecution(t, "services", borrowerKey, ""), dir); err == nil {
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

// These plans are generated and verified by Okra's provider tests. Rook only consumes the wire contract.
func testExecution(t *testing.T, fixture, owner, borrower string) Plan {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", fixture+".json"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if fixture == "allocation" {
		// The allocation name is deliberately supplied by the plan; the executor does not derive it.
		text = strings.ReplaceAll(text, "app_b1bb03dcca5d2651", "app_"+borrower[:8])
	}
	text = strings.ReplaceAll(text, "bde017fb-cc01-46c9-9c83-4558dd9f9568", owner)
	text = strings.ReplaceAll(text, "775303c8-08b3-428f-9b6a-3425265d24c4", borrower)
	var execution ResourceExecution
	if err := json.Unmarshal([]byte(text), &execution); err != nil {
		t.Fatal(err)
	}
	return Plan{Execution: &execution}
}

func TestSecretResolutionPreservesUnresolvedPlanAndDynamicComposeFields(t *testing.T) {
	plan := testExecution(t, "services", "project", "")
	original := string(plan.Execution.Compose)
	plan.Execution.Compose = json.RawMessage(strings.ReplaceAll(original, `"mysql:8.4"`, `"postgres:next","mem_limit":"128m"`))
	before := string(plan.Execution.Compose)
	if err := validateResourceExecution(plan.Execution); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	resourceDir := filepath.Join(dir, "resources", "project")
	if err := os.MkdirAll(resourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSecrets(resourceDir, plan.Execution.Secrets); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveExecution(*plan.Execution, dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(plan.Execution.Compose) != before || !strings.Contains(plan.Execution.Runtime.Env["DB_PASSWORD"], "{{secret:") {
		t.Fatal("secret resolution mutated the reusable wire plan")
	}
	if !strings.Contains(string(resolved.Compose), `"mem_limit":"128m"`) || strings.Contains(string(resolved.Compose), "{{secret:") {
		t.Fatal("Compose extensions lost or secrets unresolved")
	}
}

func TestCleanupDoesNotSilentlyIgnoreUntrackedRetainedResources(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "resources", "project"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := NewDockerManager().DeleteResources(context.Background(), "project", dir); err == nil {
		t.Fatal("retained resources without cleanup intent were reported as deleted")
	}
}
