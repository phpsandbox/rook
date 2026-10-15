package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/vmihailenco/msgpack/v5"
	"nhooyr.io/websocket"
)

type WSClient struct {
	url         string
	token       string
	conn        *websocket.Conn
	mu          sync.Mutex
	relayWrites chan struct{}
}

func NewWSClient(url, token string) *WSClient {
	return &WSClient{url: url, token: token, relayWrites: make(chan struct{}, 1)}
}

func (c *WSClient) Connect(ctx context.Context) error {
	opts := &websocket.DialOptions{
		HTTPHeader: map[string][]string{
			"Authorization": {"Bearer " + c.token},
		},
	}

	conn, _, err := websocket.Dial(ctx, c.url, opts)
	if err != nil {
		return fmt.Errorf("connect to control plane: %w", err)
	}
	conn.SetReadLimit(24 << 20)
	c.mu.Lock()
	previous := c.conn
	c.conn = conn
	c.mu.Unlock()
	if previous != nil {
		_ = previous.CloseNow()
	}
	return nil
}

func (c *WSClient) Close() error {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Close(websocket.StatusNormalClosure, "shutdown")
}

func (c *WSClient) Send(ctx context.Context, msg OutboundMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal outbound message: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("not connected")
	}

	return conn.Write(ctx, websocket.MessageText, data)
}

func (c *WSClient) SendRelayFrame(ctx context.Context, frame RelayFrame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := msgpack.Marshal(frame)
	if err != nil {
		return fmt.Errorf("marshal outbound messagepack: %w", err)
	}

	// Canceled producers can leave the queue without interrupting another
	// stream's write. Once a frame starts, let it finish atomically: canceling
	// the context passed to websocket.Write closes the shared connection.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.relayWrites <- struct{}{}:
	}
	defer func() { <-c.relayWrites }()
	c.mu.Lock()
	conn := c.conn
	contextErr := ctx.Err()
	c.mu.Unlock()
	// A canceled stream from the previous data connection must not write to
	// its replacement (a canceled websocket write can close that connection).
	if contextErr != nil {
		return contextErr
	}
	if conn == nil {
		return fmt.Errorf("not connected")
	}

	writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageBinary, data)
}

func (c *WSClient) Read(ctx context.Context) (InboundMessage, error) {
	messageType, data, err := c.ReadRaw(ctx)
	if err != nil {
		return InboundMessage{}, err
	}

	var msg InboundMessage
	switch messageType {
	case websocket.MessageText:
		if err := json.Unmarshal(data, &msg); err != nil {
			return InboundMessage{}, fmt.Errorf("unmarshal inbound message: %w", err)
		}
	case websocket.MessageBinary:
		var wire struct {
			Type      string             `msgpack:"type"`
			CommandID string             `msgpack:"commandId"`
			Payload   msgpack.RawMessage `msgpack:"payload"`
		}
		if err := msgpack.Unmarshal(data, &wire); err != nil {
			return InboundMessage{}, fmt.Errorf("unmarshal inbound messagepack: %w", err)
		}
		msg.Type = wire.Type
		msg.CommandID = wire.CommandID
		msg.messagePackPayload = wire.Payload
	default:
		return InboundMessage{}, fmt.Errorf("unsupported websocket message type %v", messageType)
	}
	if msg.Type == "" {
		return InboundMessage{}, fmt.Errorf("inbound message type is required")
	}
	return msg, nil
}

func (c *WSClient) ReadRelayFrame(ctx context.Context) (RelayFrame, error) {
	messageType, data, err := c.ReadRaw(ctx)
	if err != nil {
		return RelayFrame{}, err
	}
	if messageType != websocket.MessageBinary {
		return RelayFrame{}, fmt.Errorf("expected binary messagepack frame, got websocket message type %v", messageType)
	}
	var frame RelayFrame
	if err := msgpack.Unmarshal(data, &frame); err != nil {
		return RelayFrame{}, err
	}
	return frame, nil
}

func (c *WSClient) ReadRaw(ctx context.Context) (websocket.MessageType, []byte, error) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return 0, nil, fmt.Errorf("not connected")
	}

	return conn.Read(ctx)
}

func (c *WSClient) ConnectWithRetry(ctx context.Context) error {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		err := c.Connect(ctx)
		if err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}
