package runtime

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

//go:embed dockerfile.tmpl laravel_caddyfile.tmpl laravel_start.sh.tmpl launcher.sh
var templateFS embed.FS

type Plan struct {
	Command []string
	Port    int
}

func WriteLaravelImageFiles(workspace string, plan Plan) (string, error) {
	generatedDir := filepath.Join(workspace, ".phpsandbox", "cloudflare")
	if err := os.MkdirAll(generatedDir, 0o755); err != nil {
		return "", err
	}

	launcher, err := templateFS.ReadFile("launcher.sh")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(generatedDir, "launcher.sh"), launcher, 0o755); err != nil {
		return "", err
	}

	port := runtimePort(plan.Port)
	portData := struct{ Port int }{Port: port}
	caddyfile, err := renderTemplate("laravel_caddyfile.tmpl", portData)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(generatedDir, "Caddyfile"), caddyfile, 0o644); err != nil {
		return "", err
	}

	startScript, err := renderTemplate("laravel_start.sh.tmpl", portData)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(generatedDir, "laravel-start.sh"), startScript, 0o755); err != nil {
		return "", err
	}

	commandJSON, err := json.Marshal(plan.Command)
	if err != nil {
		return "", err
	}
	dockerfile, err := renderTemplate("dockerfile.tmpl", struct {
		Port    int
		Command string
	}{
		Port:    port,
		Command: string(commandJSON),
	})
	if err != nil {
		return "", err
	}

	dockerfilePath := filepath.Join(workspace, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, dockerfile, 0o644); err != nil {
		return "", err
	}
	dockerignorePath := filepath.Join(workspace, ".dockerignore")
	if _, err := os.Stat(dockerignorePath); os.IsNotExist(err) {
		_ = os.WriteFile(dockerignorePath, []byte(".git\nnode_modules\n"), 0o644)
	}

	return dockerfilePath, nil
}

func renderTemplate(name string, data any) ([]byte, error) {
	content, err := templateFS.ReadFile(name)
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New(name).Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func runtimePort(port int) int {
	if port <= 0 {
		return 8000
	}
	return port
}
