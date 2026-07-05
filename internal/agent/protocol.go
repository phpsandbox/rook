package agent

import (
	"encoding/json"

	"github.com/vmihailenco/msgpack/v5"
)

type InboundMessage struct {
	Type      string          `json:"type"`
	CommandID string          `json:"commandId,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	decoded   any
}

func (m InboundMessage) DecodePayload(v any) error {
	if len(m.Payload) > 0 {
		return json.Unmarshal(m.Payload, v)
	}
	if m.decoded == nil {
		return nil
	}
	data, err := msgpackMarshal(m.decoded)
	if err != nil {
		return err
	}
	return msgpackUnmarshal(data, v)
}

func msgpackMarshal(v any) ([]byte, error) {
	return msgpack.Marshal(v)
}

func msgpackUnmarshal(data []byte, v any) error {
	return msgpack.Unmarshal(data, v)
}

type OutboundMessage struct {
	Type      string `json:"type"`
	CommandID string `json:"commandId,omitempty"`

	// hello
	ServerID     string   `json:"serverId,omitempty"`
	Version      string   `json:"version,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Deployments  []string `json:"deployments,omitempty"`

	// heartbeat
	Resources *ResourceInfo `json:"resources,omitempty"`

	// log
	Stream  string `json:"stream,omitempty"`
	Content string `json:"content,omitempty"`

	// phase
	Phase string         `json:"phase,omitempty"`
	Data  map[string]any `json:"data,omitempty"`

	// result
	Success bool            `json:"success,omitempty"`
	Error   string          `json:"error,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`

	// http_response
	RequestID string            `json:"requestId,omitempty"`
	Status    int               `json:"status,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      string            `json:"body,omitempty"`
}

type ResourceInfo struct {
	CPUPercent    float64 `json:"cpuPercent"`
	MemoryUsedMB  int64   `json:"memoryUsedMb"`
	MemoryTotalMB int64   `json:"memoryTotalMb"`
	DiskUsedGB    int64   `json:"diskUsedGb"`
	DiskTotalGB   int64   `json:"diskTotalGb"`
}

type DeployPayload struct {
	DeploymentID string            `json:"deploymentId" msgpack:"deploymentId"`
	Source       SourceRef         `json:"source" msgpack:"source"`
	Plan         Plan              `json:"plan" msgpack:"plan"`
	Bundle       *DeployBundle     `json:"bundle,omitempty" msgpack:"bundle,omitempty"`
	Env          map[string]string `json:"env" msgpack:"env"`
	Options      map[string]any    `json:"options,omitempty" msgpack:"options,omitempty"`
}

type DeployBundle struct {
	Format string `json:"format" msgpack:"format"`
	Size   int64  `json:"size" msgpack:"size"`
	SHA256 string `json:"sha256" msgpack:"sha256"`
	Data   []byte `json:"data" msgpack:"data"`
}

type StopPayload struct {
	DeploymentID string `json:"deploymentId" msgpack:"deploymentId"`
}

type DeletePayload struct {
	DeploymentID string `json:"deploymentId" msgpack:"deploymentId"`
}

type LogsTailPayload struct {
	DeploymentID string `json:"deploymentId" msgpack:"deploymentId"`
	Lines        int    `json:"lines" msgpack:"lines"`
}

type HTTPRequestPayload struct {
	RequestID    string            `json:"requestId" msgpack:"requestId"`
	DeploymentID string            `json:"deploymentId" msgpack:"deploymentId"`
	Method       string            `json:"method" msgpack:"method"`
	Path         string            `json:"path" msgpack:"path"`
	Headers      map[string]string `json:"headers" msgpack:"headers"`
	Body         string            `json:"body,omitempty" msgpack:"body,omitempty"`
}
