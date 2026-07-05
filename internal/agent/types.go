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

type BuildPlan struct {
	Strategy string   `json:"strategy" msgpack:"strategy"`
	Commands []string `json:"commands" msgpack:"commands"`
	Workdir  string   `json:"workdir" msgpack:"workdir"`
}

type RuntimePlan struct {
	Provider        string            `json:"provider" msgpack:"provider"`
	Image           string            `json:"image,omitempty" msgpack:"image,omitempty"`
	Command         []string          `json:"command,omitempty" msgpack:"command,omitempty"`
	Workdir         string            `json:"workdir" msgpack:"workdir"`
	Port            int               `json:"port" msgpack:"port"`
	HealthPath      string            `json:"healthPath" msgpack:"healthPath"`
	Env             map[string]string `json:"env,omitempty" msgpack:"env,omitempty"`
	ProviderOptions any               `json:"providerOptions,omitempty" msgpack:"providerOptions,omitempty"`
}

type Plan struct {
	Strategy string      `json:"strategy" msgpack:"strategy"`
	Build    BuildPlan   `json:"build" msgpack:"build"`
	Runtime  RuntimePlan `json:"runtime" msgpack:"runtime"`
}
