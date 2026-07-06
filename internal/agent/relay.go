package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"nhooyr.io/websocket"
)

const relayChunkSize = 32 * 1024

type MessagePackSender interface {
	SendMessagePack(context.Context, any) error
}

type RelayManager struct {
	proxy *Proxy
	ws    MessagePackSender

	mu               sync.Mutex
	httpStreams      map[string]*httpRelayStream
	webSocketStreams map[string]*webSocketRelayStream
}

type httpRelayStream struct {
	writer *io.PipeWriter
}

type webSocketRelayStream struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
}

func NewRelayManager(proxy *Proxy, ws MessagePackSender) *RelayManager {
	return &RelayManager{
		proxy:            proxy,
		ws:               ws,
		httpStreams:      map[string]*httpRelayStream{},
		webSocketStreams: map[string]*webSocketRelayStream{},
	}
}

func (m *RelayManager) Handle(ctx context.Context, frame RelayFrame) {
	if err := validateInboundRelayFrame(frame); err != nil {
		m.sendReset(ctx, frame.StreamID, RelayKindHTTP, err)
		return
	}

	switch frame.Type {
	case RelayFrameOpen:
		m.handleOpen(ctx, frame)
	case RelayFrameData:
		m.handleData(ctx, frame)
	case RelayFrameEnd:
		m.handleEnd(frame)
	case RelayFrameReset:
		m.handleReset(frame)
	}
}

func (m *RelayManager) Reset(err error) {
	m.mu.Lock()
	httpStreams := m.httpStreams
	webSocketStreams := m.webSocketStreams
	m.httpStreams = map[string]*httpRelayStream{}
	m.webSocketStreams = map[string]*webSocketRelayStream{}
	m.mu.Unlock()

	for _, stream := range httpStreams {
		_ = stream.writer.CloseWithError(err)
	}
	for _, stream := range webSocketStreams {
		stream.cancel()
		_ = stream.conn.Close(websocket.StatusGoingAway, err.Error())
	}
}

func (m *RelayManager) handleOpen(ctx context.Context, frame RelayFrame) {
	switch frame.Kind {
	case RelayKindHTTP:
		if isWebSocketUpgrade(frame.Headers) {
			m.handleWebSocketOpen(ctx, frame)
			return
		}
		m.handleHTTPOpen(ctx, frame)
	default:
		m.sendReset(ctx, frame.StreamID, frame.Kind, fmt.Errorf("unsupported relay stream kind %q", frame.Kind))
	}
}

func (m *RelayManager) handleHTTPOpen(ctx context.Context, frame RelayFrame) {
	if frame.DeploymentID == "" {
		m.sendReset(ctx, frame.StreamID, frame.Kind, fmt.Errorf("deploymentId is required"))
		return
	}

	var body io.Reader = http.NoBody
	if relayFrameHasBody(frame) {
		reader, writer := io.Pipe()
		body = reader
		stream := &httpRelayStream{writer: writer}

		m.mu.Lock()
		if previous := m.httpStreams[frame.StreamID]; previous != nil {
			_ = previous.writer.CloseWithError(fmt.Errorf("stream superseded"))
		}
		m.httpStreams[frame.StreamID] = stream
		m.mu.Unlock()
	}

	go m.runHTTPRequest(ctx, frame, body)
}

func (m *RelayManager) handleWebSocketOpen(ctx context.Context, frame RelayFrame) {
	if frame.DeploymentID == "" {
		m.sendReset(ctx, frame.StreamID, frame.Kind, fmt.Errorf("deploymentId is required"))
		return
	}
	if relayFrameHasBody(frame) {
		m.sendReset(ctx, frame.StreamID, frame.Kind, fmt.Errorf("websocket upgrade must not include a request body"))
		return
	}

	targetURL, err := m.proxy.WebSocketURL(frame.DeploymentID, nonEmptyPath(frame.Path))
	if err != nil {
		m.sendReset(ctx, frame.StreamID, frame.Kind, err)
		return
	}

	headers := http.Header{}
	for _, header := range frame.Headers {
		name := strings.TrimSpace(header[0])
		if name == "" || strings.EqualFold(name, "host") || strings.EqualFold(name, "connection") || strings.EqualFold(name, "upgrade") {
			continue
		}
		headers.Add(name, header[1])
	}

	streamCtx, cancel := context.WithCancel(ctx)
	conn, _, err := websocket.Dial(streamCtx, targetURL, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		cancel()
		m.sendReset(ctx, frame.StreamID, frame.Kind, err)
		return
	}

	m.mu.Lock()
	if previous := m.webSocketStreams[frame.StreamID]; previous != nil {
		previous.cancel()
		_ = previous.conn.Close(websocket.StatusNormalClosure, "superseded")
	}
	m.webSocketStreams[frame.StreamID] = &webSocketRelayStream{conn: conn, cancel: cancel}
	m.mu.Unlock()

	_ = m.ws.SendMessagePack(ctx, RelayFrame{
		Protocol: RelayProtocol,
		Type:     RelayFrameHeaders,
		StreamID: frame.StreamID,
		Kind:     RelayKindHTTP,
		Status:   http.StatusSwitchingProtocols,
	})

	go m.readWebSocket(streamCtx, frame.StreamID, conn)
}

func (m *RelayManager) handleData(ctx context.Context, frame RelayFrame) {
	if stream := m.httpStream(frame.StreamID); stream != nil {
		if len(frame.Data) > 0 {
			_, _ = stream.writer.Write(frame.Data)
		}
		return
	}

	stream := m.webSocketStream(frame.StreamID)
	if stream == nil {
		return
	}
	messageType := websocket.MessageBinary
	if frame.Text {
		messageType = websocket.MessageText
	}
	if err := stream.conn.Write(ctx, messageType, frame.Data); err != nil {
		m.sendReset(ctx, frame.StreamID, RelayKindHTTP, err)
		m.closeWebSocket(frame.StreamID, websocket.StatusInternalError, "write failed")
	}
}

func (m *RelayManager) handleEnd(frame RelayFrame) {
	if stream := m.removeHTTP(frame.StreamID); stream != nil {
		_ = stream.writer.Close()
		return
	}

	code := websocket.StatusNormalClosure
	if frame.Code > 0 {
		code = websocket.StatusCode(frame.Code)
	}
	m.closeWebSocket(frame.StreamID, code, frame.Reason)
}

func (m *RelayManager) handleReset(frame RelayFrame) {
	message := frame.Error
	if message == "" {
		message = "stream reset"
	}
	if stream := m.removeHTTP(frame.StreamID); stream != nil {
		_ = stream.writer.CloseWithError(fmt.Errorf("%s", message))
		return
	}
	m.closeWebSocket(frame.StreamID, websocket.StatusInternalError, message)
}

func (m *RelayManager) runHTTPRequest(ctx context.Context, frame RelayFrame, body io.Reader) {
	defer m.removeHTTP(frame.StreamID)

	resp, err := m.proxy.OpenHTTP(ctx, frame.DeploymentID, nonEmptyMethod(frame.Method), nonEmptyPath(frame.Path), frame.Headers, body)
	if err != nil {
		m.sendReset(ctx, frame.StreamID, RelayKindHTTP, err)
		return
	}
	defer resp.Body.Close()

	if err := m.ws.SendMessagePack(ctx, RelayFrame{
		Protocol: RelayProtocol,
		Type:     RelayFrameHeaders,
		StreamID: frame.StreamID,
		Kind:     RelayKindHTTP,
		Status:   resp.StatusCode,
		Headers:  responseHeaderPairs(resp.Header),
	}); err != nil {
		return
	}

	buffer := make([]byte, relayChunkSize)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buffer[:n])
			if err := m.ws.SendMessagePack(ctx, RelayFrame{
				Protocol: RelayProtocol,
				Type:     RelayFrameData,
				StreamID: frame.StreamID,
				Kind:     RelayKindHTTP,
				Data:     chunk,
			}); err != nil {
				return
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			m.sendReset(ctx, frame.StreamID, RelayKindHTTP, readErr)
			return
		}
	}

	_ = m.ws.SendMessagePack(ctx, RelayFrame{
		Protocol: RelayProtocol,
		Type:     RelayFrameEnd,
		StreamID: frame.StreamID,
		Kind:     RelayKindHTTP,
	})
}

func (m *RelayManager) readWebSocket(ctx context.Context, streamID string, conn *websocket.Conn) {
	defer m.removeWebSocket(streamID)

	for {
		messageType, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			status := websocket.CloseStatus(err)
			if status < 0 {
				status = websocket.StatusAbnormalClosure
			}
			_ = m.ws.SendMessagePack(ctx, RelayFrame{
				Protocol: RelayProtocol,
				Type:     RelayFrameEnd,
				StreamID: streamID,
				Kind:     RelayKindHTTP,
				Code:     int(status),
				Reason:   err.Error(),
			})
			return
		}

		if err := m.ws.SendMessagePack(ctx, RelayFrame{
			Protocol: RelayProtocol,
			Type:     RelayFrameData,
			StreamID: streamID,
			Kind:     RelayKindHTTP,
			Data:     data,
			Text:     messageType == websocket.MessageText,
		}); err != nil {
			return
		}
	}
}

func (m *RelayManager) httpStream(streamID string) *httpRelayStream {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.httpStreams[streamID]
}

func (m *RelayManager) webSocketStream(streamID string) *webSocketRelayStream {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.webSocketStreams[streamID]
}

func (m *RelayManager) removeHTTP(streamID string) *httpRelayStream {
	m.mu.Lock()
	stream := m.httpStreams[streamID]
	delete(m.httpStreams, streamID)
	m.mu.Unlock()
	return stream
}

func (m *RelayManager) removeWebSocket(streamID string) *webSocketRelayStream {
	m.mu.Lock()
	stream := m.webSocketStreams[streamID]
	delete(m.webSocketStreams, streamID)
	m.mu.Unlock()
	if stream != nil {
		stream.cancel()
	}
	return stream
}

func (m *RelayManager) closeWebSocket(streamID string, code websocket.StatusCode, reason string) {
	stream := m.removeWebSocket(streamID)
	if stream == nil {
		return
	}
	_ = stream.conn.Close(code, reason)
}

func (m *RelayManager) sendReset(ctx context.Context, streamID string, kind string, err error) {
	if streamID == "" {
		return
	}
	_ = m.ws.SendMessagePack(ctx, RelayFrame{
		Protocol: RelayProtocol,
		Type:     RelayFrameReset,
		StreamID: streamID,
		Kind:     kind,
		Error:    err.Error(),
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

func nonEmptyMethod(method string) string {
	if strings.TrimSpace(method) == "" {
		return http.MethodGet
	}
	return method
}

func nonEmptyPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return "/"
	}
	return path
}

func validateInboundRelayFrame(frame RelayFrame) error {
	if frame.Protocol != RelayProtocol {
		return fmt.Errorf("unsupported relay protocol %q", frame.Protocol)
	}
	if strings.TrimSpace(frame.StreamID) == "" {
		return fmt.Errorf("streamId is required")
	}
	if frame.Kind != RelayKindHTTP {
		return fmt.Errorf("unsupported relay stream kind %q", frame.Kind)
	}

	switch frame.Type {
	case RelayFrameOpen:
		if strings.TrimSpace(frame.DeploymentID) == "" {
			return fmt.Errorf("deploymentId is required")
		}
		if strings.TrimSpace(frame.Method) == "" {
			return fmt.Errorf("method is required")
		}
		if strings.TrimSpace(frame.Path) == "" {
			return fmt.Errorf("path is required")
		}
		if frame.HasBody == nil {
			return fmt.Errorf("hasBody is required")
		}
	case RelayFrameData:
	case RelayFrameEnd:
	case RelayFrameReset:
		if strings.TrimSpace(frame.Error) == "" {
			return fmt.Errorf("reset error is required")
		}
	default:
		return fmt.Errorf("unsupported relay frame type %q", frame.Type)
	}
	return nil
}

func isWebSocketUpgrade(headers []HeaderPair) bool {
	for _, header := range headers {
		if strings.EqualFold(strings.TrimSpace(header[0]), "upgrade") && strings.EqualFold(strings.TrimSpace(header[1]), "websocket") {
			return true
		}
	}
	return false
}

func relayFrameHasBody(frame RelayFrame) bool {
	return frame.HasBody != nil && *frame.HasBody
}
