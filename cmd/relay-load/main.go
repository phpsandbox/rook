// relay-load measures HTTP forwarding through a real Rook process and its data
// WebSocket. The mock control plane and HTTP origin run on loopback.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/phpsandbox/rook/internal/agent"
	"github.com/phpsandbox/rook/internal/host"
	"github.com/vmihailenco/msgpack/v5"
	"nhooyr.io/websocket"
)

type profile struct {
	name string
	size int
	post bool
}

var profiles = map[string]profile{
	"get-1k":   {"get-1k", 1024, false},
	"get-64k":  {"get-64k", 64 * 1024, false},
	"get-3m":   {"get-3m", 3 * 1024 * 1024, false},
	"post-64k": {"post-64k", 64 * 1024, true},
}

type pending struct {
	done    chan struct{}
	body    []byte
	id      string
	offset  int
	headers bool
	err     string
}

type plane struct {
	conn    *websocket.Conn
	mu      sync.Mutex
	pending map[string]*pending
	fatal   error
	next    atomic.Uint64
}

type result struct {
	Profile           string         `json:"profile"`
	Concurrency       int            `json:"concurrency"`
	Elapsed           float64        `json:"elapsed_seconds"`
	Requests          int            `json:"requests"`
	Success           int            `json:"success"`
	Failures          int            `json:"failures"`
	OriginConnections uint64         `json:"new_origin_connections"`
	RPS               float64        `json:"successful_rps"`
	MiBPS             float64        `json:"response_mib_per_second"`
	P50               float64        `json:"p50_ms"`
	P95               float64        `json:"p95_ms"`
	P99               float64        `json:"p99_ms"`
	Max               float64        `json:"max_ms"`
	Errors            map[string]int `json:"errors,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	binary := flag.String("rook", "./bin/rook", "path to a compiled Rook binary")
	concurrencyFlag := flag.String("concurrency", "1,16,64,256,1024", "comma-separated worker counts")
	profileFlag := flag.String("profiles", "get-1k,get-64k,post-64k", "comma-separated workload profiles")
	duration := flag.Duration("duration", 10*time.Second, "load duration per profile/concurrency pair, excluding drain")
	warmup := flag.Duration("warmup", time.Second, "warmup duration per pair")
	timeout := flag.Duration("timeout", 10*time.Second, "per-request deadline")
	originDelay := flag.Duration("origin-delay", 0, "simulated HTTP application processing time")
	output := flag.String("output", "", "optional JSONL results file (also emitted to stdout)")
	flag.Parse()
	if *duration <= 0 || *warmup < 0 || *timeout <= 0 || *originDelay < 0 {
		return fmt.Errorf("duration and timeout must be positive; warmup and origin-delay must be nonnegative")
	}
	var counts []int
	for _, value := range strings.Split(*concurrencyFlag, ",") {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return fmt.Errorf("invalid concurrency %q", value)
		}
		counts = append(counts, n)
	}
	var selected []profile
	for _, name := range strings.Split(*profileFlag, ",") {
		p, ok := profiles[name]
		if !ok {
			return fmt.Errorf("unknown profile %q", name)
		}
		selected = append(selected, p)
	}
	writer := io.Writer(os.Stdout)
	if *output != "" {
		file, err := os.Create(*output)
		if err != nil {
			return err
		}
		defer file.Close()
		writer = io.MultiWriter(writer, file)
	}
	encoder := json.NewEncoder(writer)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	bodyByName := map[string][]byte{}
	for name, p := range profiles {
		body := make([]byte, p.size)
		for i := range body {
			body[i] = byte(i % 251)
		}
		bodyByName[name] = body
	}
	var originConnections atomic.Uint64
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		p, ok := profiles[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		expectedMethod := http.MethodGet
		if p.post {
			expectedMethod = http.MethodPost
		}
		if r.Method != expectedMethod {
			http.Error(w, "invalid request method", http.StatusMethodNotAllowed)
			return
		}
		if p.post {
			body, err := io.ReadAll(io.LimitReader(r.Body, int64(p.size+1)))
			if err != nil || !bytes.Equal(body, bodyByName[name]) {
				http.Error(w, "invalid upload", http.StatusBadRequest)
				return
			}
		}
		if *originDelay > 0 {
			timer := time.NewTimer(*originDelay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Request-ID", r.Header.Get("X-Request-ID"))
		w.Write(bodyByName[name])
	}))
	origin.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			originConnections.Add(1)
		}
	}
	origin.Start()
	defer origin.Close()
	dir, err := os.MkdirTemp("", "rook-relay-load-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	_, portString, err := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		return err
	}
	state := host.NewBindings(filepath.Join(dir, "state"))
	if err := state.Set("load-app", host.Binding{Port: port}); err != nil {
		return err
	}
	dataReady := make(chan *websocket.Conn, 1)
	helloReady := make(chan struct{}, 1)
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-load-token" {
			w.WriteHeader(401)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn.SetReadLimit(24 << 20)
		if r.URL.Query().Get("channel") == "data" {
			select {
			case dataReady <- conn:
			case <-ctx.Done():
				conn.CloseNow()
			}
			return
		}
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var message agent.OutboundMessage
			if json.Unmarshal(data, &message) == nil && message.Type == "hello" {
				select {
				case helloReady <- struct{}{}:
				default:
				}
			}
		}
	}))
	defer control.Close()
	config := fmt.Sprintf("server_id: load-server\ntoken: local-load-token\ncontrol_plane: %s/connect\nstate_dir: %s\n", control.URL, filepath.Join(dir, "state"))
	configPath := filepath.Join(dir, "rook.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		return err
	}
	cmd := exec.Command(*binary, "--config", configPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	var conn *websocket.Conn
	// Graceful shutdown also waits for the child; temporary state is then removed.
	defer func() {
		cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
		}
		if conn != nil {
			conn.CloseNow()
		}
		if stderr.Len() > 0 {
			fmt.Fprint(os.Stderr, stderr.String())
		}
	}()
	startup, stopStartup := context.WithTimeout(ctx, 15*time.Second)
	defer stopStartup()
	select {
	case conn = <-dataReady:
	case <-startup.Done():
		return fmt.Errorf("Rook data connection: %w", startup.Err())
	}
	select {
	case <-helloReady:
	case <-startup.Done():
		return fmt.Errorf("Rook hello: %w", startup.Err())
	}
	pl := &plane{conn: conn, pending: map[string]*pending{}}
	go pl.read(ctx)
	if err := encoder.Encode(map[string]any{
		"type": "environment", "time": time.Now().UTC(), "go": runtime.Version(),
		"os": runtime.GOOS, "arch": runtime.GOARCH, "cpus": runtime.NumCPU(),
		"rook_binary": *binary, "rook_pid": cmd.Process.Pid,
		"duration": duration.String(), "warmup": warmup.String(), "request_timeout": timeout.String(),
		"origin_delay": originDelay.String(),
		"topology":     "load generator -> one MessagePack data WebSocket -> compiled Rook process -> loopback HTTP origin",
	}); err != nil {
		return err
	}
	totalFailures := 0
	for _, p := range selected {
		for _, count := range counts {
			if *warmup > 0 {
				connectionsBefore := originConnections.Load()
				warm := pl.load(ctx, p, bodyByName[p.name], count, *warmup, *timeout)
				warm.OriginConnections = originConnections.Load() - connectionsBefore
				totalFailures += warm.Failures
				if warm.Failures > 0 {
					warm.Profile += "-warmup"
					if err := encoder.Encode(warm); err != nil {
						return err
					}
				}
				pl.mu.Lock()
				fatal := pl.fatal
				pl.mu.Unlock()
				if fatal != nil {
					return fmt.Errorf("data connection failed during warmup: %w", fatal)
				}
			}
			connectionsBefore := originConnections.Load()
			r := pl.load(ctx, p, bodyByName[p.name], count, *duration, *timeout)
			r.OriginConnections = originConnections.Load() - connectionsBefore
			if err := encoder.Encode(r); err != nil {
				return err
			}
			totalFailures += r.Failures
			pl.mu.Lock()
			fatal := pl.fatal
			pl.mu.Unlock()
			if fatal != nil {
				return fmt.Errorf("data connection failed: %w", fatal)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	if totalFailures > 0 {
		return fmt.Errorf("load test recorded %d failures", totalFailures)
	}
	return nil
}

func (p *plane) write(ctx context.Context, frame agent.RelayFrame) error {
	data, err := msgpack.Marshal(frame)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return p.conn.Write(writeCtx, websocket.MessageBinary, data)
}

func (p *plane) request(ctx context.Context, workload profile, body []byte, timeout time.Duration) string {
	id := strconv.FormatUint(p.next.Add(1), 10)
	req := &pending{done: make(chan struct{}), body: body, id: id}
	p.mu.Lock()
	if p.fatal != nil {
		err := p.fatal.Error()
		p.mu.Unlock()
		return err
	}
	p.pending[id] = req
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, id); p.mu.Unlock() }()
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	method := http.MethodGet
	if workload.post {
		method = http.MethodPost
	}
	frame := agent.RelayFrame{
		Protocol: agent.RelayProtocol, Type: agent.RelayFrameOpen,
		StreamID: id, Kind: agent.RelayKindHTTP, DeploymentID: "load-app",
		Method: method, Path: "/" + workload.name,
		Headers: []agent.HeaderPair{{"X-Request-ID", id}}, HasBody: &workload.post,
	}
	if err := p.write(requestCtx, frame); err != nil {
		return "write open: " + err.Error()
	}
	if workload.post {
		for offset := 0; offset < len(body); offset += 32 * 1024 {
			end := min(offset+32*1024, len(body))
			// Data frames only carry the stream identity and payload.
			if err := p.write(requestCtx, agent.RelayFrame{Protocol: agent.RelayProtocol, Type: agent.RelayFrameData, StreamID: id, Kind: agent.RelayKindHTTP, Data: body[offset:end]}); err != nil {
				return "write data: " + err.Error()
			}
		}
		if err := p.write(requestCtx, agent.RelayFrame{Protocol: agent.RelayProtocol, Type: agent.RelayFrameEnd, StreamID: id, Kind: agent.RelayKindHTTP}); err != nil {
			return "write end: " + err.Error()
		}
	}
	select {
	case <-req.done:
		return req.err
	case <-requestCtx.Done():
		return "request deadline/cancellation"
	}
}

func (p *plane) read(ctx context.Context) {
	for {
		kind, data, err := p.conn.Read(ctx)
		var frame agent.RelayFrame
		if err == nil && kind != websocket.MessageBinary {
			err = fmt.Errorf("nonbinary response")
		}
		if err == nil {
			err = msgpack.Unmarshal(data, &frame)
		}
		p.mu.Lock()
		if err != nil {
			p.fatal = err
			for id, req := range p.pending {
				req.err = "connection: " + err.Error()
				close(req.done)
				delete(p.pending, id)
			}
			p.mu.Unlock()
			return
		}
		req := p.pending[frame.StreamID]
		if req == nil {
			p.mu.Unlock()
			continue
		}
		if frame.Protocol != agent.RelayProtocol || frame.Kind != agent.RelayKindHTTP {
			req.err = "invalid response protocol/kind"
		}
		switch frame.Type {
		case agent.RelayFrameHeaders:
			if req.headers || frame.Status != http.StatusOK {
				req.err = "invalid/duplicate response status"
			}
			req.headers = true
			found := false
			for _, header := range frame.Headers {
				if strings.EqualFold(header[0], "X-Request-ID") && header[1] == req.id {
					found = true
				}
			}
			if !found {
				req.err = "missing/mismatched request ID header"
			}
		case agent.RelayFrameData:
			end := req.offset + len(frame.Data)
			if !req.headers || end > len(req.body) || !bytes.Equal(frame.Data, req.body[req.offset:min(end, len(req.body))]) {
				req.err = "response body mismatch/order"
			} else {
				req.offset = end
			}
		case agent.RelayFrameEnd:
			if !req.headers || req.offset != len(req.body) {
				req.err = "incomplete response"
			}
			close(req.done)
			delete(p.pending, frame.StreamID)
		case agent.RelayFrameReset:
			req.err = "reset: " + frame.Error
			close(req.done)
			delete(p.pending, frame.StreamID)
		default:
			req.err = "unexpected response frame"
		}
		p.mu.Unlock()
	}
}

func (p *plane) load(ctx context.Context, workload profile, body []byte, concurrency int, duration, timeout time.Duration) result {
	r := result{Profile: workload.name, Concurrency: concurrency, Errors: map[string]int{}}
	var mu sync.Mutex
	var latencies []float64
	var workers sync.WaitGroup
	start := time.Now()
	deadline := start.Add(duration)
	for i := 0; i < concurrency; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for time.Now().Before(deadline) && ctx.Err() == nil {
				requestStart := time.Now()
				err := p.request(ctx, workload, body, timeout)
				latency := float64(time.Since(requestStart)) / float64(time.Millisecond)
				mu.Lock()
				r.Requests++
				if err == "" {
					r.Success++
					latencies = append(latencies, latency)
				} else {
					r.Failures++
					r.Errors[err]++
				}
				mu.Unlock()
				p.mu.Lock()
				fatal := p.fatal
				p.mu.Unlock()
				if fatal != nil {
					return
				}
			}
		}()
	}
	workers.Wait()
	r.Elapsed = time.Since(start).Seconds()
	r.RPS = float64(r.Success) / r.Elapsed
	r.MiBPS = r.RPS * float64(workload.size) / (1024 * 1024)
	sort.Float64s(latencies)
	if len(latencies) > 0 {
		percentile := func(q float64) float64 { return latencies[int(float64(len(latencies)-1)*q)] }
		r.P50, r.P95, r.P99, r.Max = percentile(.5), percentile(.95), percentile(.99), latencies[len(latencies)-1]
	}
	return r
}
