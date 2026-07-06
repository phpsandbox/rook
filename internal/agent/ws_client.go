package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

type WSClient struct {
	url   string
	token string
	conn  *websocket.Conn
	mu    sync.Mutex
}

func NewWSClient(url, token string) *WSClient {
	return &WSClient{url: url, token: token}
}

func (c *WSClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	opts := &websocket.DialOptions{
		HTTPHeader: map[string][]string{
			"Authorization": {"Bearer " + c.token},
		},
	}

	conn, _, err := websocket.Dial(ctx, c.url, opts)
	if err != nil {
		return fmt.Errorf("connect to control plane: %w", err)
	}
	c.conn = conn
	return nil
}

func (c *WSClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	return c.conn.Close(websocket.StatusNormalClosure, "shutdown")
}

func (c *WSClient) Send(ctx context.Context, msg OutboundMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal outbound message: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	conn := c.conn
	if conn == nil {
		return fmt.Errorf("not connected")
	}

	return conn.Write(ctx, websocket.MessageText, data)
}

func (c *WSClient) SendMessagePack(ctx context.Context, msg any) error {
	data, err := msgpackMarshal(msg)
	if err != nil {
		return fmt.Errorf("marshal outbound messagepack: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	conn := c.conn
	if conn == nil {
		return fmt.Errorf("not connected")
	}

	return conn.Write(ctx, websocket.MessageBinary, data)
}

func (c *WSClient) Read(ctx context.Context) (InboundMessage, error) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return InboundMessage{}, fmt.Errorf("not connected")
	}

	messageType, data, err := conn.Read(ctx)
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
			Type      string `msgpack:"type"`
			CommandID string `msgpack:"commandId"`
			Payload   any    `msgpack:"payload"`
		}
		if err := msgpackUnmarshal(data, &wire); err != nil {
			return InboundMessage{}, fmt.Errorf("unmarshal inbound messagepack: %w", err)
		}
		msg.Type = wire.Type
		msg.CommandID = wire.CommandID
		msg.decoded = wire.Payload
	default:
		return InboundMessage{}, fmt.Errorf("unsupported websocket message type %v", messageType)
	}
	if msg.Type == "" {
		return InboundMessage{}, fmt.Errorf("inbound message type is required")
	}
	return msg, nil
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
