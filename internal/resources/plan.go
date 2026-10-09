package resources

import "encoding/json"

const ExecutionCapability = "resource-execution.v1"

// Plan is a resolved host plan. Resource engines and application defaults belong to the control plane.
type Plan struct {
	Key          string             `json:"key" msgpack:"key"`
	Project      string             `json:"project" msgpack:"project"`
	Secrets      []string           `json:"secrets,omitempty" msgpack:"secrets,omitempty"`
	Dependencies map[string]string  `json:"dependencies,omitempty" msgpack:"dependencies,omitempty"`
	Compose      json.RawMessage    `json:"compose" msgpack:"compose"`
	Runtime      Runtime            `json:"runtime" msgpack:"runtime"`
	Prepare      []Operation        `json:"prepare,omitempty" msgpack:"prepare,omitempty"`
	Ownership    map[string]Cleanup `json:"ownership" msgpack:"ownership"`
	Release      []string           `json:"release,omitempty" msgpack:"release,omitempty"`
}

type Runtime struct {
	Network string            `json:"network" msgpack:"network"`
	Mounts  map[string]string `json:"mounts" msgpack:"mounts"`
	Env     map[string]string `json:"env" msgpack:"env"`
}

type Operation struct {
	Action    string            `json:"action" msgpack:"action"`
	Scope     string            `json:"scope" msgpack:"scope"`
	Container string            `json:"container" msgpack:"container"`
	Command   []string          `json:"command,omitempty" msgpack:"command,omitempty"`
	Env       map[string]string `json:"env,omitempty" msgpack:"env,omitempty"`
	Input     string            `json:"input,omitempty" msgpack:"input,omitempty"`
	Network   string            `json:"network,omitempty" msgpack:"network,omitempty"`
	Alias     string            `json:"alias,omitempty" msgpack:"alias,omitempty"`
}

// Cleanup is an explicit host cleanup instruction owned by a stable ID.
// Omitting an ID from a later plan retains it; release explicitly executes and forgets it.
type Cleanup struct {
	Project      string            `json:"project,omitempty" msgpack:"project,omitempty"`
	Compose      json.RawMessage   `json:"compose,omitempty" msgpack:"compose,omitempty"`
	Volume       string            `json:"volume,omitempty" msgpack:"volume,omitempty"`
	Network      string            `json:"network,omitempty" msgpack:"network,omitempty"`
	Dependencies map[string]string `json:"dependencies,omitempty" msgpack:"dependencies,omitempty"`
	Operations   []Operation       `json:"operations,omitempty" msgpack:"operations,omitempty"`
}
