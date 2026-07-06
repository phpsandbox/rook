package agent

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"nhooyr.io/websocket"
)

type WebSocketTunnelManager struct {
	proxy   *Proxy
	ws      MessagePackSender
	mu      sync.Mutex
	streams map[string]*webSocketTunnelStream
}

type webSocketTunnelStream struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
}

func NewWebSocketTunnelManager(proxy *Proxy, ws MessagePackSender) *WebSocketTunnelManager {
	return &WebSocketTunnelManager{
		proxy:   proxy,
		ws:      ws,
		streams: map[string]*webSocketTunnelStream{},
	}
}

func IsWebSocketTunnelMessage(messageType string) bool {
	switch messageType {
	case "websocket.start", "websocket.message", "websocket.close":
		return true
	default:
		return false
	}
}

func (m *WebSocketTunnelManager) Handle(ctx context.Context, msg InboundMessage) {
	switch msg.Type {
	case "websocket.start":
		m.handleStart(ctx, msg)
	case "websocket.message":
		m.handleMessage(ctx, msg)
	case "websocket.close":
		m.handleClose(ctx, msg)
	}
}

func (m *WebSocketTunnelManager) handleStart(ctx context.Context, msg InboundMessage) {
	var payload WebSocketStartPayload
	if err := msg.DecodePayload(&payload); err != nil {
		m.sendWebSocketError(ctx, msg.CommandID, "", err)
		return
	}
	if payload.RequestID == "" {
		m.sendWebSocketError(ctx, msg.CommandID, "", fmt.Errorf("requestId is required"))
		return
	}

	targetURL, err := m.proxy.WebSocketURL(payload.DeploymentID, payload.Path)
	if err != nil {
		m.sendWebSocketError(ctx, msg.CommandID, payload.RequestID, err)
		return
	}

	headers := http.Header{}
	for _, header := range payload.Headers {
		name := strings.TrimSpace(header[0])
		if name == "" || strings.EqualFold(name, "host") {
			continue
		}
		headers.Add(name, header[1])
	}

	streamCtx, cancel := context.WithCancel(ctx)
	conn, _, err := websocket.Dial(streamCtx, targetURL, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		cancel()
		m.sendWebSocketError(ctx, msg.CommandID, payload.RequestID, err)
		return
	}

	m.mu.Lock()
	if previous := m.streams[payload.RequestID]; previous != nil {
		previous.cancel()
		_ = previous.conn.Close(websocket.StatusNormalClosure, "superseded")
	}
	m.streams[payload.RequestID] = &webSocketTunnelStream{conn: conn, cancel: cancel}
	m.mu.Unlock()

	_ = m.ws.SendMessagePack(ctx, HTTPTunnelMessage{
		Type:      "websocket.open",
		CommandID: msg.CommandID,
		RequestID: payload.RequestID,
	})

	go m.readWebSocket(streamCtx, msg.CommandID, payload.RequestID, conn)
}

func (m *WebSocketTunnelManager) handleMessage(ctx context.Context, msg InboundMessage) {
	var payload WebSocketMessagePayload
	if err := msg.DecodePayload(&payload); err != nil {
		return
	}
	stream := m.stream(payload.RequestID)
	if stream == nil {
		return
	}

	messageType := websocket.MessageBinary
	if payload.Text {
		messageType = websocket.MessageText
	}
	if err := stream.conn.Write(ctx, messageType, payload.Data); err != nil {
		m.sendWebSocketError(ctx, msg.CommandID, payload.RequestID, err)
		m.remove(payload.RequestID)
	}
}

func (m *WebSocketTunnelManager) handleClose(ctx context.Context, msg InboundMessage) {
	var payload WebSocketClosePayload
	if err := msg.DecodePayload(&payload); err != nil {
		return
	}

	code := websocket.StatusNormalClosure
	if payload.Code > 0 {
		code = websocket.StatusCode(payload.Code)
	}
	stream := m.stream(payload.RequestID)
	if stream != nil {
		_ = stream.conn.Close(code, payload.Reason)
	}
	m.remove(payload.RequestID)
}

func (m *WebSocketTunnelManager) readWebSocket(ctx context.Context, commandID string, requestID string, conn *websocket.Conn) {
	defer m.remove(requestID)

	for {
		messageType, data, err := conn.Read(ctx)
		if err != nil {
			status := websocket.CloseStatus(err)
			if status < 0 {
				status = websocket.StatusAbnormalClosure
			}
			_ = m.ws.SendMessagePack(ctx, HTTPTunnelMessage{
				Type:      "websocket.close",
				CommandID: commandID,
				RequestID: requestID,
				Code:      int(status),
				Reason:    err.Error(),
			})
			return
		}

		if err := m.ws.SendMessagePack(ctx, HTTPTunnelMessage{
			Type:      "websocket.message",
			CommandID: commandID,
			RequestID: requestID,
			Data:      data,
			Text:      messageType == websocket.MessageText,
		}); err != nil {
			return
		}
	}
}

func (m *WebSocketTunnelManager) stream(requestID string) *webSocketTunnelStream {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streams[requestID]
}

func (m *WebSocketTunnelManager) remove(requestID string) {
	m.mu.Lock()
	stream := m.streams[requestID]
	delete(m.streams, requestID)
	m.mu.Unlock()
	if stream != nil {
		stream.cancel()
	}
}

func (m *WebSocketTunnelManager) sendWebSocketError(ctx context.Context, commandID string, requestID string, err error) {
	_ = m.ws.SendMessagePack(ctx, HTTPTunnelMessage{
		Type:      "websocket.error",
		CommandID: commandID,
		RequestID: requestID,
		Error:     err.Error(),
	})
}
