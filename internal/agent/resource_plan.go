package agent

import "encoding/json"

const ResourceExecutionCapability = "resource-execution.v1"

// ResourceExecution is a resolved host plan. Resource engines and application defaults belong to the control plane.
type ResourceExecution struct {
	Key          string              `json:"key" msgpack:"key"`
	Project      string              `json:"project" msgpack:"project"`
	Secrets      []string            `json:"secrets,omitempty" msgpack:"secrets,omitempty"`
	Dependencies map[string]string   `json:"dependencies,omitempty" msgpack:"dependencies,omitempty"`
	Compose      json.RawMessage     `json:"compose" msgpack:"compose"`
	Runtime      ResourceRuntime     `json:"runtime" msgpack:"runtime"`
	Prepare      []ResourceOperation `json:"prepare,omitempty" msgpack:"prepare,omitempty"`
	Cleanup      []ResourceOperation `json:"cleanup,omitempty" msgpack:"cleanup,omitempty"`
}

type ResourceRuntime struct {
	Network string            `json:"network" msgpack:"network"`
	Mounts  map[string]string `json:"mounts" msgpack:"mounts"`
	Env     map[string]string `json:"env" msgpack:"env"`
}

type ResourceOperation struct {
	Action    string            `json:"action" msgpack:"action"`
	Scope     string            `json:"scope" msgpack:"scope"`
	Container string            `json:"container" msgpack:"container"`
	Command   []string          `json:"command,omitempty" msgpack:"command,omitempty"`
	Env       map[string]string `json:"env,omitempty" msgpack:"env,omitempty"`
	Input     string            `json:"input,omitempty" msgpack:"input,omitempty"`
	Network   string            `json:"network,omitempty" msgpack:"network,omitempty"`
	Alias     string            `json:"alias,omitempty" msgpack:"alias,omitempty"`
}

type ComposeDocument struct {
	Services map[string]json.RawMessage `json:"services" msgpack:"services"`
	Networks map[string]ComposeNetwork  `json:"networks" msgpack:"networks"`
	Volumes  map[string]ComposeVolume   `json:"volumes" msgpack:"volumes"`
}

type ComposeNetwork struct {
	Name string `json:"name" msgpack:"name"`
}
type ComposeVolume struct {
	Name string `json:"name" msgpack:"name"`
}
