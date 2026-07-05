package agent

import "encoding/json"

const (
	StrategyLaravel = "laravel"

	SourceProviderGit  = "git"
	SourceProviderPath = "path"
)

type SourceRef struct {
	Provider    string `json:"provider"`
	GitURL      string `json:"gitUrl,omitempty"`
	Ref         string `json:"ref,omitempty"`
	Path        string `json:"path,omitempty"`
	GitUsername string `json:"gitUsername,omitempty"`
	GitPassword string `json:"gitPassword,omitempty"`
	GitToken    string `json:"gitToken,omitempty"`
}

type BuildPlan struct {
	Strategy string   `json:"strategy"`
	Commands []string `json:"commands"`
	Workdir  string   `json:"workdir"`
}

type RuntimePlan struct {
	Provider        string            `json:"provider"`
	Image           string            `json:"image,omitempty"`
	Command         []string          `json:"command,omitempty"`
	Workdir         string            `json:"workdir"`
	Port            int               `json:"port"`
	HealthPath      string            `json:"healthPath"`
	Env             map[string]string `json:"env,omitempty"`
	ProviderOptions json.RawMessage   `json:"providerOptions,omitempty"`
}

type Plan struct {
	Strategy string      `json:"strategy"`
	Build    BuildPlan   `json:"build"`
	Runtime  RuntimePlan `json:"runtime"`
}
