package agent

import "encoding/json"

type InboundMessage struct {
	Type      string          `json:"type"`
	CommandID string          `json:"commandId,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
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
	DeploymentID string            `json:"deploymentId"`
	Source       json.RawMessage   `json:"source"`
	Plan         json.RawMessage   `json:"plan"`
	Env          map[string]string `json:"env"`
	Options      json.RawMessage   `json:"options"`
}

type StopPayload struct {
	DeploymentID string `json:"deploymentId"`
}

type DeletePayload struct {
	DeploymentID string `json:"deploymentId"`
}

type LogsTailPayload struct {
	DeploymentID string `json:"deploymentId"`
	Lines        int    `json:"lines"`
}

type HTTPRequestPayload struct {
	RequestID    string            `json:"requestId"`
	DeploymentID string            `json:"deploymentId"`
	Method       string            `json:"method"`
	Path         string            `json:"path"`
	Headers      map[string]string `json:"headers"`
	Body         string            `json:"body,omitempty"`
}
