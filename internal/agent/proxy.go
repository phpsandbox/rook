package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type Proxy struct {
	state *StateStore
}

func NewProxy(state *StateStore) *Proxy {
	return &Proxy{state: state}
}

func (p *Proxy) HandleHTTP(deploymentID string, method string, path string, headers map[string]string, body string) (int, map[string]string, string, error) {
	headerPairs := make([]HeaderPair, 0, len(headers))
	for k, v := range headers {
		headerPairs = append(headerPairs, HeaderPair{k, v})
	}

	resp, err := p.OpenHTTP(context.Background(), deploymentID, method, path, headerPairs, strings.NewReader(body))
	if err != nil {
		return 502, nil, "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	respHeaders := map[string]string{}
	for k := range resp.Header {
		respHeaders[k] = resp.Header.Get(k)
	}

	return resp.StatusCode, respHeaders, string(respBody), nil
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
		req.Header.Add(name, value)
	}

	resp, err := http.DefaultClient.Do(req)
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
