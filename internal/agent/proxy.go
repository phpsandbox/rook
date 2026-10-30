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

	"github.com/phpsandbox/rook/internal/host"
)

const maxConcurrentHTTPRequests = 64
const maxQueuedHTTPRequests = 1024

var errHTTPRelayOverloaded = errors.New("HTTP relay at capacity")

type Proxy struct {
	state           BindingResolver
	client          *http.Client
	webSocketClient *http.Client
	requests        chan struct{}
	waiting         chan struct{}
}

type BindingResolver interface {
	Resolve(context.Context, string) (host.Binding, error)
}

func NewProxy(state BindingResolver) *Proxy {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 256
	transport.MaxIdleConnsPerHost = maxConcurrentHTTPRequests
	transport.MaxConnsPerHost = maxConcurrentHTTPRequests
	transport.ResponseHeaderTimeout = 30 * time.Second
	webSocketTransport := transport.Clone()
	// WebSocket workers enforce their own limits, including pending handshakes.
	// Their long-lived connections must not reserve HTTP transport slots.
	webSocketTransport.MaxConnsPerHost = 0
	return &Proxy{
		state:    state,
		requests: make(chan struct{}, maxConcurrentHTTPRequests),
		waiting:  make(chan struct{}, maxQueuedHTTPRequests),
		webSocketClient: &http.Client{Transport: webSocketTransport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (p *Proxy) OpenHTTP(ctx context.Context, deploymentID string, method string, path string, headers []HeaderPair, body io.Reader) (*http.Response, error) {
	// The HTTP client owns the body after dispatch; close it ourselves when
	// admission or runtime inspection fails before it can take ownership.
	dispatched := false
	defer func() {
		if !dispatched {
			if closer, ok := body.(io.Closer); ok {
				_ = closer.Close()
			}
		}
	}()
	// Admission also bounds concurrent Docker inspections.
	if err := p.acquire(ctx, body != nil && body != http.NoBody); err != nil {
		return nil, err
	}
	release := true
	defer func() {
		if release {
			<-p.requests
		}
	}()
	ds, err := p.state.Resolve(ctx, deploymentID)
	if err != nil {
		return nil, err
	}

	targetURL := fmt.Sprintf("http://127.0.0.1:%d%s", ds.Port, normalizeProxyPath(path))
	if body == nil {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, method, targetURL, body)
	if err != nil {
		return nil, fmt.Errorf("create proxy request: %w", err)
	}
	for _, header := range headers {
		name := strings.TrimSpace(header[0])
		if name == "" {
			continue
		}
		value := header[1]
		if strings.EqualFold(name, "host") {
			req.Host = value
			continue
		}
		if strings.EqualFold(name, "accept-encoding") {
			continue
		}
		req.Header.Add(name, value)
	}

	dispatched = true
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("proxy request failed: %w", err)
	}
	// Keep the slot until the caller closes the response.
	release = false
	resp.Body = &proxyResponseBody{ReadCloser: resp.Body, release: func() { <-p.requests }}
	return resp, nil
}

func (p *Proxy) acquire(ctx context.Context, hasBody bool) error {
	if hasBody {
		// The relay reads uploads through pipes on its shared receive loop.
		// A queued upload would block that loop before active uploads can finish.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case p.requests <- struct{}{}:
			return nil
		default:
			return errHTTPRelayOverloaded
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case p.requests <- struct{}{}:
		return nil
	default:
	}
	// Bound the waiting population as well as the active upstream sockets.
	select {
	case p.waiting <- struct{}{}:
	default:
		return errHTTPRelayOverloaded
	}
	defer func() { <-p.waiting }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case p.requests <- struct{}{}:
		return nil
	}
}

type proxyResponseBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
	err     error
}

func (b *proxyResponseBody) Close() error {
	b.once.Do(func() {
		b.err = b.ReadCloser.Close()
		b.release()
	})
	return b.err
}

func (p *Proxy) WebSocketURL(ctx context.Context, deploymentID string, path string) (string, error) {
	ds, err := p.state.Resolve(ctx, deploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ws://127.0.0.1:%d%s", ds.Port, normalizeProxyPath(path)), nil
}

func normalizeProxyPath(path string) string {
	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
}
