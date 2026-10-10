package agent

import (
	"encoding/json"
	"github.com/phpsandbox/rook/internal/host"

	"github.com/vmihailenco/msgpack/v5"
)

type InboundMessage struct {
	Type               string          `json:"type"`
	CommandID          string          `json:"commandId,omitempty"`
	Payload            json.RawMessage `json:"payload,omitempty"`
	messagePackPayload msgpack.RawMessage
}

func (m InboundMessage) DecodeHostRequest() (host.Request, error) {
	var request host.Request
	if len(m.Payload) > 0 {
		return request, json.Unmarshal(m.Payload, &request)
	}
	return request, msgpack.Unmarshal(m.messagePackPayload, &request)
}

type OutboundMessage struct {
	Type      string `json:"type"`
	CommandID string `json:"commandId,omitempty"`

	// hello
	ServerID     string   `json:"serverId,omitempty"`
	Version      string   `json:"version,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Deployments  []string `json:"deployments,omitempty"`

	// log
	Stream  string `json:"stream,omitempty"`
	Content string `json:"content,omitempty"`

	// result
	Success bool            `json:"success,omitempty"`
	Error   string          `json:"error,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
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
