package agent

const (
	StrategyLaravel = "laravel"

	SourceProviderGit  = "git"
	SourceProviderPath = "path"
)

type SourceRef struct {
	Provider    string `json:"provider" msgpack:"provider"`
	GitURL      string `json:"gitUrl,omitempty" msgpack:"gitUrl,omitempty"`
	Ref         string `json:"ref,omitempty" msgpack:"ref,omitempty"`
	Path        string `json:"path,omitempty" msgpack:"path,omitempty"`
	GitUsername string `json:"gitUsername,omitempty" msgpack:"gitUsername,omitempty"`
	GitPassword string `json:"gitPassword,omitempty" msgpack:"gitPassword,omitempty"`
	GitToken    string `json:"gitToken,omitempty" msgpack:"gitToken,omitempty"`
}

type RuntimePlan struct {
	Command    []string `json:"command,omitempty" msgpack:"command,omitempty"`
	Port       int      `json:"port" msgpack:"port"`
	HealthPath string   `json:"healthPath" msgpack:"healthPath"`
}

type Plan struct {
	Runtime RuntimePlan `json:"runtime" msgpack:"runtime"`
}
