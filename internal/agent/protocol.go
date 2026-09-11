package agent

import (
	"encoding/json"

	"github.com/vmihailenco/msgpack/v5"
)

type InboundMessage struct {
	Type               string          `json:"type"`
	CommandID          string          `json:"commandId,omitempty"`
	Payload            json.RawMessage `json:"payload,omitempty"`
	messagePackPayload msgpack.RawMessage
}

func (m InboundMessage) DecodeDeployPayload() (DeployPayload, error) {
	var payload DeployPayload
	if len(m.Payload) > 0 {
		return payload, json.Unmarshal(m.Payload, &payload)
	}
	if len(m.messagePackPayload) > 0 {
		return payload, msgpack.Unmarshal(m.messagePackPayload, &payload)
	}
	return payload, nil
}

func (m InboundMessage) DecodeStopPayload() (StopPayload, error) {
	var payload StopPayload
	if len(m.Payload) > 0 {
		return payload, json.Unmarshal(m.Payload, &payload)
	}
	if len(m.messagePackPayload) > 0 {
		return payload, msgpack.Unmarshal(m.messagePackPayload, &payload)
	}
	return payload, nil
}

func (m InboundMessage) DecodeDeletePayload() (DeletePayload, error) {
	var payload DeletePayload
	if len(m.Payload) > 0 {
		return payload, json.Unmarshal(m.Payload, &payload)
	}
	if len(m.messagePackPayload) > 0 {
		return payload, msgpack.Unmarshal(m.messagePackPayload, &payload)
	}
	return payload, nil
}

func (m InboundMessage) DecodeLogsTailPayload() (LogsTailPayload, error) {
	var payload LogsTailPayload
	if len(m.Payload) > 0 {
		return payload, json.Unmarshal(m.Payload, &payload)
	}
	if len(m.messagePackPayload) > 0 {
		return payload, msgpack.Unmarshal(m.messagePackPayload, &payload)
	}
	return payload, nil
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
	Phase string           `json:"phase,omitempty"`
	Data  *DeployPhaseData `json:"data,omitempty"`

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
	Manifest     DeployManifest    `json:"manifest" msgpack:"manifest"`
	Plan         Plan              `json:"plan" msgpack:"plan"`
	Bundle       *DeployBundle     `json:"bundle,omitempty" msgpack:"bundle,omitempty"`
	Env          map[string]string `json:"env" msgpack:"env"`
}

type DeployManifest struct {
	SchemaVersion int                 `json:"schemaVersion" msgpack:"schemaVersion"`
	Build         DeployManifestBuild `json:"build" msgpack:"build"`
}

type DeployManifestBuild struct {
	KeepWorkspace bool `json:"keepWorkspace" msgpack:"keepWorkspace"`
}

type DeployPhaseData struct {
	Port        int    `json:"port"`
	ContainerID string `json:"containerId"`
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

const (
	RelayProtocol = "okra.relay.v1"

	RelayFrameOpen    = "stream.open"
	RelayFrameHeaders = "stream.headers"
	RelayFrameData    = "stream.data"
	RelayFrameEnd     = "stream.end"
	RelayFrameReset   = "stream.reset"

	RelayKindHTTP = "http"
)

type RelayFrame struct {
	Protocol string `json:"protocol,omitempty" msgpack:"protocol,omitempty"`
	Type     string `json:"type" msgpack:"type"`
	StreamID string `json:"streamId" msgpack:"streamId"`
	Kind     string `json:"kind,omitempty" msgpack:"kind,omitempty"`

	DeploymentID string       `json:"deploymentId,omitempty" msgpack:"deploymentId,omitempty"`
	Method       string       `json:"method,omitempty" msgpack:"method,omitempty"`
	Path         string       `json:"path,omitempty" msgpack:"path,omitempty"`
	Headers      []HeaderPair `json:"headers,omitempty" msgpack:"headers,omitempty"`
	HasBody      *bool        `json:"hasBody,omitempty" msgpack:"hasBody,omitempty"`
	Status       int          `json:"status,omitempty" msgpack:"status,omitempty"`

	Data   []byte `json:"data,omitempty" msgpack:"data,omitempty"`
	Text   bool   `json:"text,omitempty" msgpack:"text,omitempty"`
	Code   int    `json:"code,omitempty" msgpack:"code,omitempty"`
	Reason string `json:"reason,omitempty" msgpack:"reason,omitempty"`
	Error  string `json:"error,omitempty" msgpack:"error,omitempty"`
}
