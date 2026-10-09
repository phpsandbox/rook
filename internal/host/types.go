package host

import "encoding/json"

const Capability = "host-execution.v1"

type Request struct {
	ResolveSecrets bool                `json:"resolveSecrets,omitempty" msgpack:"resolveSecrets,omitempty"`
	BindingID      string              `json:"bindingId,omitempty" msgpack:"bindingId,omitempty"`
	Binding        *Binding            `json:"binding,omitempty" msgpack:"binding,omitempty"`
	ID             string              `json:"id" msgpack:"id"`
	Action         string              `json:"action" msgpack:"action"`
	Command        []string            `json:"command,omitempty" msgpack:"command,omitempty"`
	Directory      string              `json:"directory,omitempty" msgpack:"directory,omitempty"`
	Env            map[string]string   `json:"env,omitempty" msgpack:"env,omitempty"`
	Input          string              `json:"input,omitempty" msgpack:"input,omitempty"`
	Path           string              `json:"path,omitempty" msgpack:"path,omitempty"`
	Data           []byte              `json:"data,omitempty" msgpack:"data,omitempty"`
	Executable     bool                `json:"executable,omitempty" msgpack:"executable,omitempty"`
	Secrets        map[string][]string `json:"secrets,omitempty" msgpack:"secrets,omitempty"`
	TimeoutSeconds int                 `json:"timeoutSeconds,omitempty" msgpack:"timeoutSeconds,omitempty"`
}

type Result struct {
	Status string          `json:"status"`
	Output string          `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	Digest string          `json:"digest,omitempty"`
}
