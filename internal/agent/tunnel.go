package agent

import (
	"context"
	"fmt"
	"io"
	"sync"
)

const httpTunnelChunkSize = 32 * 1024

type HTTPTunnelManager struct {
	proxy   *Proxy
	ws      MessagePackSender
	mu      sync.Mutex
	streams map[string]*httpTunnelStream
}

type MessagePackSender interface {
	SendMessagePack(context.Context, any) error
}

type httpTunnelStream struct {
	writer *io.PipeWriter
}

func NewHTTPTunnelManager(proxy *Proxy, ws MessagePackSender) *HTTPTunnelManager {
	return &HTTPTunnelManager{
		proxy:   proxy,
		ws:      ws,
		streams: map[string]*httpTunnelStream{},
	}
}

func IsHTTPTunnelMessage(messageType string) bool {
	switch messageType {
	case "http.start", "http.body", "http.end", "http.cancel":
		return true
	default:
		return false
	}
}

func (m *HTTPTunnelManager) Handle(ctx context.Context, msg InboundMessage) {
	switch msg.Type {
	case "http.start":
		m.handleStart(ctx, msg)
	case "http.body":
		m.handleBody(msg)
	case "http.end":
		m.handleEnd(msg)
	case "http.cancel":
		m.handleCancel(msg)
	}
}

func (m *HTTPTunnelManager) handleStart(ctx context.Context, msg InboundMessage) {
	var payload HTTPStartPayload
	if err := msg.DecodePayload(&payload); err != nil {
		m.sendError(ctx, msg.CommandID, "", err)
		return
	}
	if payload.RequestID == "" {
		m.sendError(ctx, msg.CommandID, "", fmt.Errorf("requestId is required"))
		return
	}

	reader, writer := io.Pipe()
	stream := &httpTunnelStream{writer: writer}

	m.mu.Lock()
	if previous := m.streams[payload.RequestID]; previous != nil {
		_ = previous.writer.CloseWithError(fmt.Errorf("request superseded"))
	}
	m.streams[payload.RequestID] = stream
	m.mu.Unlock()

	go m.runRequest(ctx, msg.CommandID, payload, reader)
}

func (m *HTTPTunnelManager) handleBody(msg InboundMessage) {
	var payload HTTPBodyPayload
	if err := msg.DecodePayload(&payload); err != nil {
		return
	}

	stream := m.stream(payload.RequestID)
	if stream == nil || len(payload.Data) == 0 {
		return
	}
	_, _ = stream.writer.Write(payload.Data)
}

func (m *HTTPTunnelManager) handleEnd(msg InboundMessage) {
	var payload HTTPEndPayload
	if err := msg.DecodePayload(&payload); err != nil {
		return
	}

	stream := m.stream(payload.RequestID)
	if stream == nil {
		return
	}
	_ = stream.writer.Close()
}

func (m *HTTPTunnelManager) handleCancel(msg InboundMessage) {
	var payload HTTPEndPayload
	if err := msg.DecodePayload(&payload); err != nil {
		return
	}

	stream := m.stream(payload.RequestID)
	if stream == nil {
		return
	}
	_ = stream.writer.CloseWithError(fmt.Errorf("request cancelled"))
}

func (m *HTTPTunnelManager) runRequest(ctx context.Context, commandID string, payload HTTPStartPayload, body io.Reader) {
	defer m.remove(payload.RequestID)

	resp, err := m.proxy.OpenHTTP(ctx, payload.DeploymentID, payload.Method, payload.Path, payload.Headers, body)
	if err != nil {
		m.sendError(ctx, commandID, payload.RequestID, err)
		return
	}
	defer resp.Body.Close()

	if err := m.ws.SendMessagePack(ctx, HTTPTunnelMessage{
		Type:      "http.response.start",
		CommandID: commandID,
		RequestID: payload.RequestID,
		Status:    resp.StatusCode,
		Headers:   responseHeaderPairs(resp.Header),
	}); err != nil {
		return
	}

	buffer := make([]byte, httpTunnelChunkSize)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buffer[:n])
			if err := m.ws.SendMessagePack(ctx, HTTPTunnelMessage{
				Type:      "http.response.body",
				CommandID: commandID,
				RequestID: payload.RequestID,
				Data:      chunk,
			}); err != nil {
				return
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			m.sendError(ctx, commandID, payload.RequestID, readErr)
			return
		}
	}

	_ = m.ws.SendMessagePack(ctx, HTTPTunnelMessage{
		Type:      "http.response.end",
		CommandID: commandID,
		RequestID: payload.RequestID,
	})
}

func (m *HTTPTunnelManager) stream(requestID string) *httpTunnelStream {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streams[requestID]
}

func (m *HTTPTunnelManager) remove(requestID string) {
	m.mu.Lock()
	stream := m.streams[requestID]
	delete(m.streams, requestID)
	m.mu.Unlock()
	if stream != nil {
		_ = stream.writer.Close()
	}
}

func (m *HTTPTunnelManager) sendError(ctx context.Context, commandID string, requestID string, err error) {
	_ = m.ws.SendMessagePack(ctx, HTTPTunnelMessage{
		Type:      "http.response.error",
		CommandID: commandID,
		RequestID: requestID,
		Error:     err.Error(),
	})
}

func responseHeaderPairs(headers map[string][]string) []HeaderPair {
	pairs := make([]HeaderPair, 0, len(headers))
	for name, values := range headers {
		for _, value := range values {
			pairs = append(pairs, HeaderPair{name, value})
		}
	}
	return pairs
}
