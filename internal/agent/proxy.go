package agent

import (
	"context"
	"fmt"
	"github.com/phpsandbox/rook/internal/host"
	"io"
	"net/http"
	"strings"
)

type Proxy struct {
	state *host.Bindings
}

func NewProxy(state *host.Bindings) *Proxy {
	return &Proxy{state: state}
}

func (p *Proxy) OpenHTTP(ctx context.Context, deploymentID string, method string, path string, headers []HeaderPair, body io.Reader) (*http.Response, error) {
	ds, ok := p.state.Get(deploymentID)
	if !ok {
		return nil, fmt.Errorf("deployment %s not found", deploymentID)
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

	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("proxy request failed: %w", err)
	}
	return resp, nil
}

func (p *Proxy) WebSocketURL(deploymentID string, path string) (string, error) {
	ds, ok := p.state.Get(deploymentID)
	if !ok {
		return "", fmt.Errorf("deployment %s not found", deploymentID)
	}
	return fmt.Sprintf("ws://127.0.0.1:%d%s", ds.Port, normalizeProxyPath(path)), nil
}

func normalizeProxyPath(path string) string {
	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
}
