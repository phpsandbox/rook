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
}

type DeployBundle struct {
	Format string `json:"format" msgpack:"format"`
	Layout string `json:"layout,omitempty" msgpack:"layout,omitempty"`
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

type HeaderPair [2]string

type HTTPStartPayload struct {
	RequestID    string       `json:"requestId" msgpack:"requestId"`
	DeploymentID string       `json:"deploymentId" msgpack:"deploymentId"`
	Method       string       `json:"method" msgpack:"method"`
	Path         string       `json:"path" msgpack:"path"`
	Headers      []HeaderPair `json:"headers,omitempty" msgpack:"headers,omitempty"`
}

type HTTPBodyPayload struct {
	RequestID string `json:"requestId" msgpack:"requestId"`
	Data      []byte `json:"data" msgpack:"data"`
}

type HTTPEndPayload struct {
	RequestID string `json:"requestId" msgpack:"requestId"`
}

type WebSocketStartPayload struct {
	RequestID    string       `json:"requestId" msgpack:"requestId"`
	DeploymentID string       `json:"deploymentId" msgpack:"deploymentId"`
	Path         string       `json:"path" msgpack:"path"`
	Headers      []HeaderPair `json:"headers,omitempty" msgpack:"headers,omitempty"`
}

type WebSocketMessagePayload struct {
	RequestID string `json:"requestId" msgpack:"requestId"`
	Data      []byte `json:"data" msgpack:"data"`
	Text      bool   `json:"text" msgpack:"text"`
}

type WebSocketClosePayload struct {
	RequestID string `json:"requestId" msgpack:"requestId"`
	Code      int    `json:"code,omitempty" msgpack:"code,omitempty"`
	Reason    string `json:"reason,omitempty" msgpack:"reason,omitempty"`
}

type HTTPTunnelMessage struct {
	Type      string `json:"type" msgpack:"type"`
	CommandID string `json:"commandId,omitempty" msgpack:"commandId,omitempty"`

	RequestID string       `json:"requestId,omitempty" msgpack:"requestId,omitempty"`
	Status    int          `json:"status,omitempty" msgpack:"status,omitempty"`
	Headers   []HeaderPair `json:"headers,omitempty" msgpack:"headers,omitempty"`
	Data      []byte       `json:"data,omitempty" msgpack:"data,omitempty"`
	Text      bool         `json:"text,omitempty" msgpack:"text,omitempty"`
	Code      int          `json:"code,omitempty" msgpack:"code,omitempty"`
	Reason    string       `json:"reason,omitempty" msgpack:"reason,omitempty"`
	Error     string       `json:"error,omitempty" msgpack:"error,omitempty"`
}
