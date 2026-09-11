package agent

import (
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	DefaultConfigPath = "/etc/rook/rook.yaml"
	DefaultStateDir   = "/var/lib/rook/state"
	PortRangeStart    = 10000
	PortRangeEnd      = 32767
)

type Config struct {
	ServerID     string `yaml:"server_id" json:"serverId"`
	Token        string `yaml:"token" json:"token"`
	ControlPlane string `yaml:"control_plane" json:"controlPlane"`
	StateDir     string `yaml:"state_dir" json:"stateDir"`
}

func LoadConfig(path string) (Config, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultConfigPath
	}

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
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}

	return cfg, nil
}
