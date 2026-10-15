package agent

import (
	"fmt"
	"net/url"
	"os"

	"go.yaml.in/yaml/v3"
)

const (
	DefaultConfigPath = "/etc/rook/rook.yaml"
	DefaultStateDir   = "/var/lib/rook/state"
)

type Config struct {
	ServerID                  string `yaml:"server_id" json:"serverId"`
	Token                     string `yaml:"token" json:"token"`
	ControlPlane              string `yaml:"control_plane" json:"controlPlane"`
	StateDir                  string `yaml:"state_dir" json:"stateDir"`
	AllowInsecureControlPlane bool   `yaml:"allow_insecure_control_plane" json:"allowInsecureControlPlane"`
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read agent config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse agent config: %w", err)
	}

	if cfg.ServerID == "" {
		return Config{}, fmt.Errorf("server_id is required in agent config")
	}
	if cfg.Token == "" {
		return Config{}, fmt.Errorf("token is required in agent config")
	}
	if cfg.ControlPlane == "" {
		return Config{}, fmt.Errorf("control_plane is required in agent config")
	}
	if err := validateControlPlaneURL(cfg.ControlPlane, cfg.AllowInsecureControlPlane); err != nil {
		return Config{}, err
	}
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}

	return cfg, nil
}

func validateControlPlaneURL(rawURL string, allowInsecure bool) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("control_plane must be an absolute URL without user credentials or a fragment")
	}
	switch parsed.Scheme {
	case "https", "wss":
		return nil
	case "http", "ws":
		if allowInsecure {
			return nil
		}
		return fmt.Errorf("control_plane requires HTTPS or WSS; allow_insecure_control_plane is only for development")
	default:
		return fmt.Errorf("unsupported control_plane URL scheme")
	}
}
