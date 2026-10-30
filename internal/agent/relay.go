package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

const relayChunkSize = 32 * 1024
const maxRelayBufferedBytes = 256 * 1024
const maxRelayBufferedFrames = 64

var errRelayBufferFull = errors.New("relay stream buffer at capacity")
var errRelayIdle = errors.New("relay stream idle timeout")

type MessagePackSender interface {
	SendRelayFrame(context.Context, RelayFrame) error
}

type relayLimits struct {
	webSockets              int
	webSocketsPerDeployment int
	handshakeTimeout        time.Duration
	writeTimeout            time.Duration
	idleTimeout             time.Duration
}

type RelayManager struct {
	proxy  *Proxy
	ws     MessagePackSender
	limits relayLimits

	mu                   sync.Mutex
	streams              map[string]*relayStream
	workers              map[*relayStream]struct{}
	httpWorkers          int
	webSocketWorkers     int
	deploymentWebSockets map[string]int
	generation           context.Context
	cancelGeneration     context.CancelFunc
	resetWrites          chan struct{}
}

type relayStream struct {
	id             string
	deploymentID   string
	webSocket      bool
	hasBody        bool
	ctx            context.Context
	parent         context.Context
	generation     context.Context
	cancel         context.CancelCauseFunc
	stopGeneration func() bool

	mu            sync.Mutex
	incoming      chan RelayFrame
	bufferedBytes int
	ended         bool
	idle          *time.Timer
	idleTimeout   time.Duration
}

func NewRelayManager(proxy *Proxy, ws MessagePackSender) *RelayManager {
	generation, cancel := context.WithCancel(context.Background())
	return &RelayManager{
		proxy: proxy, ws: ws,
		limits:  relayLimits{webSockets: 256, webSocketsPerDeployment: 64, handshakeTimeout: 15 * time.Second, writeTimeout: 10 * time.Second, idleTimeout: 2 * time.Minute},
		streams: map[string]*relayStream{}, workers: map[*relayStream]struct{}{}, deploymentWebSockets: map[string]int{},
		generation: generation, cancelGeneration: cancel,
		resetWrites: make(chan struct{}, maxConcurrentHTTPRequests+maxQueuedHTTPRequests+256),
	}
}

// Handle only admits streams and enqueues frames. Upstream I/O never runs on
// the shared data receiver, and queues reject overflow instead of blocking it.
func (m *RelayManager) Handle(ctx context.Context, frame RelayFrame) {
	if err := validateInboundRelayFrame(frame); err != nil {
		m.mu.Lock()
		stream := m.streams[frame.StreamID]
		m.mu.Unlock()
		if stream != nil {
			m.failStream(stream, err)
		} else {
			m.sendReset(ctx, frame.StreamID, RelayKindHTTP, err)
		}
		return
	}
	if frame.Type == RelayFrameOpen {
		m.handleOpen(ctx, frame)
		return
	}
	m.mu.Lock()
	stream := m.streams[frame.StreamID]
	if frame.Type == RelayFrameReset && stream != nil {
		delete(m.streams, frame.StreamID)
	}
	m.mu.Unlock()
	if stream == nil {
		return
	}
	if frame.Type == RelayFrameReset {
		stream.cancel(context.Canceled)
		return
	}
	if !stream.webSocket && !stream.hasBody {
		return
	}
	if !stream.enqueue(frame) {
		m.failStream(stream, errRelayBufferFull)
	}
}

func (m *RelayManager) Reset(err error) {
	m.mu.Lock()
	workers := make([]*relayStream, 0, len(m.workers))
	for stream := range m.workers {
		workers = append(workers, stream)
	}
	m.streams = map[string]*relayStream{}
	m.cancelGeneration()
	m.generation, m.cancelGeneration = context.WithCancel(context.Background())
	m.mu.Unlock()
	for _, stream := range workers {
		stream.cancel(err)
	}
	// Workers close their own upstream sockets. Counters stay reserved until
	// those workers exit, including handshakes canceled during a reconnect.
}

func (m *RelayManager) handleOpen(ctx context.Context, frame RelayFrame) {
	isWebSocket := isWebSocketUpgrade(frame.Headers)
	if isWebSocket && relayFrameHasBody(frame) {
		m.sendReset(ctx, frame.StreamID, frame.Kind, fmt.Errorf("websocket upgrade must not include a request body"))
		return
	}
	m.mu.Lock()
	if m.streams[frame.StreamID] != nil {
		m.mu.Unlock()
		// A duplicate cannot replace a stream still producing frames with this ID.
		return
	}
	atCapacity := m.httpWorkers >= maxConcurrentHTTPRequests+maxQueuedHTTPRequests
	if isWebSocket {
		atCapacity = m.webSocketWorkers >= m.limits.webSockets || m.deploymentWebSockets[frame.DeploymentID] >= m.limits.webSocketsPerDeployment
	}
	if atCapacity {
		m.mu.Unlock()
		m.sendReset(ctx, frame.StreamID, frame.Kind, fmt.Errorf("relay at capacity"))
		return
	}
	streamCtx, cancel := context.WithCancelCause(ctx)
	stream := &relayStream{
		id: frame.StreamID, deploymentID: frame.DeploymentID, webSocket: isWebSocket, hasBody: relayFrameHasBody(frame),
		ctx: streamCtx, parent: ctx, generation: m.generation, cancel: cancel,
		idleTimeout: m.limits.idleTimeout,
	}
	if stream.hasBody || isWebSocket {
		stream.incoming = make(chan RelayFrame, maxRelayBufferedFrames)
	}
	stream.stopGeneration = context.AfterFunc(m.generation, func() { cancel(context.Canceled) })
	stream.idle = time.AfterFunc(stream.idleTimeout, func() { cancel(errRelayIdle) })
	m.streams[frame.StreamID] = stream
	m.workers[stream] = struct{}{}
	if isWebSocket {
		m.webSocketWorkers++
		m.deploymentWebSockets[frame.DeploymentID]++
	} else {
		m.httpWorkers++
	}
	m.mu.Unlock()
	if isWebSocket {
		go m.runWebSocket(frame, stream)
	} else {
		go m.runHTTPRequest(frame, stream)
	}
}

func (s *relayStream) enqueue(frame RelayFrame) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil || s.ended {
		return true
	}
	if len(frame.Data) > maxRelayBufferedBytes-s.bufferedBytes {
		return false
	}
	// Retain only fields consumed by the stream, rather than arbitrary headers
	// or other metadata attached to an otherwise valid data frame.
	frame = RelayFrame{Type: frame.Type, Data: frame.Data, Text: frame.Text, Code: frame.Code, Reason: frame.Reason}
	select {
	case s.incoming <- frame:
		s.bufferedBytes += len(frame.Data)
		s.ended = frame.Type == RelayFrameEnd
		s.idle.Reset(s.idleTimeout)
		return true
	default:
		return false
	}
}

func (s *relayStream) consumed(frame RelayFrame) {
	s.mu.Lock()
	s.bufferedBytes -= len(frame.Data)
	s.mu.Unlock()
}

func (s *relayStream) touch() {
	s.mu.Lock()
	s.idle.Reset(s.idleTimeout)
	s.mu.Unlock()
}

func (m *RelayManager) finishStream(s *relayStream) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.streams[s.id] != s {
		return false
	}
	delete(m.streams, s.id)
	return true
}

func (m *RelayManager) releaseStream(s *relayStream) {
	m.finishStream(s)
	s.cancel(context.Canceled)
	s.stopGeneration()
	s.mu.Lock()
	s.idle.Stop()
	s.mu.Unlock()
	m.mu.Lock()
	delete(m.workers, s)
	if s.webSocket {
		m.webSocketWorkers--
		m.deploymentWebSockets[s.deploymentID]--
		if m.deploymentWebSockets[s.deploymentID] == 0 {
			delete(m.deploymentWebSockets, s.deploymentID)
		}
	} else {
		m.httpWorkers--
	}
	m.mu.Unlock()
}

func (m *RelayManager) failStream(s *relayStream, err error) {
	if !m.finishStream(s) {
		return
	}
	if cause := context.Cause(s.ctx); cause != nil {
		err = cause
	}
	s.cancel(err)
	if !errors.Is(err, context.Canceled) {
		m.queueReset(s.parent, s.generation, s.id, RelayKindHTTP, err)
	}
}

// The HTTP transport reads this bounded inbox directly, preserving data/end
// order without a pipe write on the receiver or one goroutine per data frame.
type relayRequestBody struct {
	stream  *relayStream
	current []byte
	closed  chan struct{}
	once    sync.Once
	ended   bool
}

func (b *relayRequestBody) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if b.ended {
		return 0, io.EOF
	}
	for len(b.current) == 0 {
		select {
		case <-b.closed:
			return 0, io.ErrClosedPipe
		case <-b.stream.ctx.Done():
			return 0, context.Cause(b.stream.ctx)
		case frame := <-b.stream.incoming:
			b.stream.consumed(frame)
			if frame.Type == RelayFrameEnd {
				b.ended = true
				return 0, io.EOF
			}
			b.current = frame.Data
		}
	}
	n := copy(data, b.current)
	b.current = b.current[n:]
	return n, nil
}
func (b *relayRequestBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

func (m *RelayManager) runHTTPRequest(frame RelayFrame, stream *relayStream) {
	defer m.releaseStream(stream)
	var body io.Reader = http.NoBody
	if stream.hasBody {
		body = &relayRequestBody{stream: stream, closed: make(chan struct{})}
	}
	resp, err := m.proxy.OpenHTTP(stream.ctx, frame.DeploymentID, frame.Method, frame.Path, frame.Headers, body)
	if err != nil {
		m.failStream(stream, err)
		return
	}
	defer resp.Body.Close()
	stream.touch()
	if err := m.ws.SendRelayFrame(stream.ctx, RelayFrame{
		Protocol: RelayProtocol, Type: RelayFrameHeaders, StreamID: frame.StreamID, Kind: RelayKindHTTP,
		Status: resp.StatusCode, Headers: responseHeaderPairs(resp.Header),
	}); err != nil {
		m.failStream(stream, err)
		return
	}
	buffer := make([]byte, relayChunkSize)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			stream.touch()
			chunk := append([]byte(nil), buffer[:n]...)
			if err := m.ws.SendRelayFrame(stream.ctx, RelayFrame{Protocol: RelayProtocol, Type: RelayFrameData, StreamID: frame.StreamID, Kind: RelayKindHTTP, Data: chunk}); err != nil {
				m.failStream(stream, err)
				return
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			m.failStream(stream, readErr)
			return
		}
	}
	_ = resp.Body.Close()
	// Retire before the terminal frame so the peer may immediately reuse the ID.
	if m.finishStream(stream) {
		_ = m.ws.SendRelayFrame(stream.ctx, RelayFrame{Protocol: RelayProtocol, Type: RelayFrameEnd, StreamID: frame.StreamID, Kind: RelayKindHTTP})
	}
}

func (m *RelayManager) runWebSocket(frame RelayFrame, stream *relayStream) {
	defer m.releaseStream(stream)
	targetURL, err := m.proxy.WebSocketURL(stream.ctx, frame.DeploymentID, frame.Path)
	if err != nil {
		m.failStream(stream, err)
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
	dialCtx, stopDial := context.WithTimeout(stream.ctx, m.limits.handshakeTimeout)
	conn, _, err := websocket.Dial(dialCtx, targetURL, &websocket.DialOptions{HTTPHeader: headers, HTTPClient: m.proxy.webSocketClient})
	stopDial()
	if err != nil {
		m.failStream(stream, err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxRelayBufferedBytes)
	stream.touch()
	if err := m.ws.SendRelayFrame(stream.ctx, RelayFrame{Protocol: RelayProtocol, Type: RelayFrameHeaders, StreamID: frame.StreamID, Kind: RelayKindHTTP, Status: http.StatusSwitchingProtocols}); err != nil {
		m.failStream(stream, err)
		return
	}
	readDone := make(chan struct{})
	go func() { defer close(readDone); m.readWebSocket(stream, conn) }()
	defer func() { stream.cancel(context.Canceled); conn.CloseNow(); <-readDone }()
	for {
		select {
		case <-stream.ctx.Done():
			m.failStream(stream, context.Cause(stream.ctx))
			return
		case <-readDone:
			return
		case incoming := <-stream.incoming:
			stream.consumed(incoming)
			if incoming.Type == RelayFrameEnd {
				m.finishStream(stream)
				code := websocket.StatusNormalClosure
				if incoming.Code > 0 {
					code = websocket.StatusCode(incoming.Code)
				}
				_ = conn.Close(code, incoming.Reason)
				return
			}
			messageType := websocket.MessageBinary
			if incoming.Text {
				messageType = websocket.MessageText
			}
			writeCtx, cancel := context.WithTimeout(stream.ctx, m.limits.writeTimeout)
			err := conn.Write(writeCtx, messageType, incoming.Data)
			cancel()
			if err != nil {
				m.failStream(stream, err)
				return
			}
			stream.touch()
		}
	}
}

func (m *RelayManager) readWebSocket(stream *relayStream, conn *websocket.Conn) {
	for {
		messageType, data, err := conn.Read(stream.ctx)
		if err != nil {
			if stream.ctx.Err() != nil {
				m.failStream(stream, context.Cause(stream.ctx))
				return
			}
			status := websocket.CloseStatus(err)
			if status < 0 {
				status = websocket.StatusAbnormalClosure
			}
			if m.finishStream(stream) {
				_ = m.ws.SendRelayFrame(stream.ctx, RelayFrame{Protocol: RelayProtocol, Type: RelayFrameEnd, StreamID: stream.id, Kind: RelayKindHTTP, Code: int(status), Reason: err.Error()})
			}
			return
		}
		stream.touch()
		if err := m.ws.SendRelayFrame(stream.ctx, RelayFrame{Protocol: RelayProtocol, Type: RelayFrameData, StreamID: stream.id, Kind: RelayKindHTTP, Data: data, Text: messageType == websocket.MessageText}); err != nil {
			m.failStream(stream, err)
			return
		}
	}
}

func (m *RelayManager) sendReset(ctx context.Context, streamID string, kind string, err error) {
	m.mu.Lock()
	generation := m.generation
	m.mu.Unlock()
	m.queueReset(ctx, generation, streamID, kind, err)
}

func (m *RelayManager) queueReset(parent, generation context.Context, streamID string, kind string, err error) {
	if streamID == "" || parent.Err() != nil || generation.Err() != nil {
		return
	}
	// Rejections must not block the receiver either. Bound outstanding reset
	// notifications so a flood of invalid frames cannot create unlimited workers.
	select {
	case m.resetWrites <- struct{}{}:
	default:
		return
	}
	go func() {
		defer func() { <-m.resetWrites }()
		ctx, cancel := context.WithCancel(generation)
		stop := context.AfterFunc(parent, cancel)
		defer stop()
		defer cancel()
		_ = m.ws.SendRelayFrame(ctx, RelayFrame{Protocol: RelayProtocol, Type: RelayFrameReset, StreamID: streamID, Kind: kind, Error: err.Error()})
	}()
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

func validateInboundRelayFrame(frame RelayFrame) error {
	if frame.StreamID == "" || len(frame.StreamID) > 256 {
		return fmt.Errorf("relay stream ID must contain 1 to 256 bytes")
	}
	if frame.Type == RelayFrameOpen && (frame.DeploymentID == "" || len(frame.DeploymentID) > 256) {
		return fmt.Errorf("relay deployment ID must contain 1 to 256 bytes")
	}
	if len(frame.Data) > maxRelayBufferedBytes {
		return fmt.Errorf("relay frame data exceeds limit")
	}
	if frame.Type != RelayFrameData && len(frame.Data) != 0 {
		return fmt.Errorf("relay data is only valid in stream.data frames")
	}
	if len(frame.Path) > 16<<10 || len(frame.Method) > 32 || len(frame.Headers) > 256 || len(frame.Reason) > 123 || len(frame.Error) > 4096 {
		return fmt.Errorf("relay frame metadata exceeds limit")
	}
	headerBytes := 0
	for _, header := range frame.Headers {
		headerBytes += len(header[0]) + len(header[1])
		if headerBytes > 64<<10 {
			return fmt.Errorf("relay headers exceed limit")
		}
	}
	if frame.Protocol != RelayProtocol {
		return fmt.Errorf("unsupported relay protocol %q", frame.Protocol)
	}
	if frame.Kind != RelayKindHTTP {
		return fmt.Errorf("unsupported relay stream kind %q", frame.Kind)
	}

	switch frame.Type {
	case RelayFrameOpen:
	case RelayFrameData:
	case RelayFrameEnd:
	case RelayFrameReset:
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
