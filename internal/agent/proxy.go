package agent

import (
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
	ds, ok := p.state.Get(deploymentID)
	if !ok {
		return 502, nil, "", fmt.Errorf("deployment %s not found", deploymentID)
	}

	targetURL := fmt.Sprintf("http://127.0.0.1:%d%s", ds.Port, path)
	req, err := http.NewRequest(method, targetURL, strings.NewReader(body))
	if err != nil {
		return 502, nil, "", fmt.Errorf("create proxy request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 502, nil, "", fmt.Errorf("proxy request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	respHeaders := map[string]string{}
	for k := range resp.Header {
		respHeaders[k] = resp.Header.Get(k)
	}

	return resp.StatusCode, respHeaders, string(respBody), nil
}
