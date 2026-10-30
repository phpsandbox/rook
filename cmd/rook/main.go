package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/phpsandbox/rook/internal/agent"
	"github.com/phpsandbox/rook/internal/host"
	"github.com/phpsandbox/rook/internal/update"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "releases" {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		if err := runReleases(ctx, os.Args[2:], os.Stdout, os.Stderr); err != nil {
			if !errors.Is(err, flag.ErrHelp) {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
		}
		return
	}
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: rook [options] | rook releases <list|inspect|restore> [options]")
		flag.PrintDefaults()
	}

	configPath := flag.String("config", agent.DefaultConfigPath, "path to rook config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	manualUpdates := flag.Bool("manual-updates", false, "require owner approval for patch updates instead of updating automatically")
	applyUpdate := flag.Bool("apply-update", false, "run the privileged systemd update helper")
	showCapabilities := flag.Bool("capabilities", false, "print supported capabilities without connecting")
	flag.Parse()
	if *showCapabilities {
		if err := json.NewEncoder(os.Stdout).Encode([]string{host.Capability, host.RecoveryFenceCapability}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if *showVersion {
		fmt.Println(version)
		return
	}
	if *applyUpdate {
		if os.Geteuid() != 0 || *manualUpdates {
			fmt.Fprintln(os.Stderr, "error: --apply-update requires root and cannot be combined with --manual-updates")
			os.Exit(1)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		if err := update.NewUpdater().Apply(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, *manualUpdates, update.SocketPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg agent.Config, manualUpdates bool, updateSocket string) error {
	autoUpdate := !manualUpdates
	state := host.NewBindings(cfg.StateDir)
	if err := state.Load(); err != nil {
		return fmt.Errorf("load bindings: %w", err)
	}
	executor := &host.Executor{Directory: cfg.StateDir, Bindings: state}
	if err := executor.Recover(); err != nil {
		return fmt.Errorf("recover operations: %w", err)
	}
	runtimeBindings := host.NewRuntimeBindings(state)
	defer runtimeBindings.Close()
	proxy := agent.NewProxy(runtimeBindings)
	controlWS := agent.NewWSClient(agentPlaneURL(cfg.ControlPlane, cfg.ServerID, "control"), cfg.Token, cfg.AllowInsecureControlPlane)
	dataWS := agent.NewWSClient(agentPlaneURL(cfg.ControlPlane, cfg.ServerID, "data"), cfg.Token, cfg.AllowInsecureControlPlane)
	relay := agent.NewRelayManager(proxy, dataWS)
	coordinator := update.NewCoordinator(version, relay.Drain, relay.Resume)
	if err := coordinator.Enable(cfg.StateDir, autoUpdate); err != nil {
		return err
	}
	closeUpdates, err := coordinator.Serve(ctx, updateSocket)
	if err != nil {
		return err
	}
	defer closeUpdates()

	fmt.Printf("rook %s connecting to %s (server: %s)\n", version, cfg.ControlPlane, cfg.ServerID)

	if err := controlWS.ConnectWithRetry(ctx); err != nil {
		return fmt.Errorf("connect control plane: %w", err)
	}
	defer controlWS.Close()

	if err := dataWS.ConnectWithRetry(ctx); err != nil {
		return fmt.Errorf("connect data plane: %w", err)
	}
	defer dataWS.Close()

	if err := sendHello(ctx, controlWS, cfg.ServerID, state.IDs()); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	coordinator.ControlReady(true)
	coordinator.DataReady(true)

	fmt.Println("connected, waiting for commands...")
	stopTelemetry := startTelemetry(ctx, controlWS, coordinator, autoUpdate)
	defer stopTelemetry()

	go runRelayLoop(ctx, dataWS, relay, coordinator)

	for {
		msg, err := controlWS.Read(ctx)
		if err != nil {
			coordinator.ControlReady(false)
			if ctx.Err() != nil {
				return nil
			}
			fmt.Fprintf(os.Stderr, "read error: %v, reconnecting...\n", err)
			if err := controlWS.ConnectWithRetry(ctx); err != nil {
				return err
			}
			if err := sendHello(ctx, controlWS, cfg.ServerID, state.IDs()); err != nil {
				return fmt.Errorf("send reconnect hello: %w", err)
			}
			coordinator.ControlReady(true)
			continue
		}

		if msg.Type == "agent.update" {
			handleAgentUpdate(ctx, msg, controlWS, coordinator)
			continue
		}
		if !coordinator.BeginCommand() {
			deliveryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_ = controlWS.Send(deliveryCtx, agent.OutboundMessage{Type: "result", CommandID: msg.CommandID, Error: "agent updating; retry later"})
			cancel()
			continue
		}
		go func() {
			defer coordinator.EndCommand()
			handleCommand(ctx, msg, controlWS, executor)
		}()
	}
}

func sendHello(ctx context.Context, ws *agent.WSClient, serverID string, deployments []string) error {
	return ws.Send(ctx, agent.OutboundMessage{
		Type:         "hello",
		ServerID:     serverID,
		Version:      version,
		Capabilities: []string{host.Capability, host.RecoveryFenceCapability},
		Deployments:  deployments,
	})
}

func runRelayLoop(ctx context.Context, ws *agent.WSClient, relay *agent.RelayManager, readiness *update.Coordinator) {
	defer relay.Reset(context.Canceled)
	for {
		frame, err := ws.ReadRelayFrame(ctx)
		if err != nil {
			if readiness != nil {
				readiness.DataReady(false)
			}
			if ctx.Err() != nil {
				return
			}
			relay.Reset(fmt.Errorf("data plane disconnected: %w", err))
			fmt.Fprintf(os.Stderr, "data read error: %v, reconnecting...\n", err)
			if err := ws.ConnectWithRetry(ctx); err != nil {
				return
			}
			if readiness != nil {
				readiness.DataReady(true)
			}
			continue
		}

		relay.Handle(ctx, frame)
	}
}

func agentPlaneURL(rawURL string, serverID string, channel string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	}
	query := parsed.Query()
	query.Set("server_id", serverID)
	query.Set("channel", channel)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func handleCommand(ctx context.Context, msg agent.InboundMessage, ws *agent.WSClient, executor *host.Executor) {
	send := func(out agent.OutboundMessage) {
		out.CommandID = msg.CommandID
		deliveryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_ = ws.Send(deliveryCtx, out)
	}
	request, err := msg.DecodeHostRequest()
	if err != nil {
		send(agent.OutboundMessage{Type: "result", Error: err.Error()})
		return
	}
	var result host.Result
	switch msg.Type {
	case "host":
		result, err = executeHostCommand(ctx, executor, request, send)
	case "host.status":
		result, err = executor.Status(request.ID)
	default:
		err = fmt.Errorf("unsupported command %q", msg.Type)
	}
	if err != nil {
		send(agent.OutboundMessage{Type: "result", Error: err.Error()})
		return
	}
	data, err := json.Marshal(result)
	if err != nil {
		send(agent.OutboundMessage{Type: "result", Error: err.Error()})
		return
	}
	send(agent.OutboundMessage{Type: "result", Success: result.Status == "completed", Error: result.Error, Result: data})
}

func executeHostCommand(ctx context.Context, executor *host.Executor, request host.Request, send func(agent.OutboundMessage)) (host.Result, error) {
	// Delivery is best effort. A disconnected or slow control plane must never
	// prevent command pipes from draining and the terminal record from saving.
	logs := make(chan string, 128)
	go func() {
		for output := range logs {
			send(agent.OutboundMessage{Type: "log", Stream: "host", Content: output})
		}
	}()
	result, err := executor.Execute(ctx, request, func(output string) {
		select {
		case logs <- output:
		default:
		}
	})
	close(logs)
	return result, err
}

func startTelemetry(parent context.Context, ws *agent.WSClient, coordinator *update.Coordinator, automatic bool) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			err := ws.Send(ctx, agent.OutboundMessage{
				Type: "heartbeat", Version: version,
				Agent: &agent.AgentInfo{OS: runtime.GOOS, Architecture: runtime.GOARCH, ManagedUpdates: true, AutoUpdate: automatic, Operation: coordinator.Operation()},
			})
			if err != nil && ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, "agent heartbeat delivery failed")
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}

func handleAgentUpdate(ctx context.Context, msg agent.InboundMessage, ws *agent.WSClient, coordinator *update.Coordinator) {
	var request struct {
		ID            string `json:"id"`
		TargetVersion string `json:"targetVersion"`
	}
	err := json.Unmarshal(msg.Payload, &request)
	var operation update.Operation
	if err == nil {
		operation, err = coordinator.Request(request.ID, request.TargetVersion)
	}
	result := agent.OutboundMessage{Type: "result", CommandID: msg.CommandID}
	if err != nil {
		result.Error = err.Error()
	} else {
		result.Success = true
		result.Result, err = json.Marshal(operation)
		if err != nil {
			result.Success = false
			result.Error = err.Error()
		}
	}
	if err := ws.Send(ctx, result); err != nil {
		fmt.Fprintln(os.Stderr, "agent update acknowledgement delivery failed")
	}
}
