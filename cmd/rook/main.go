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
	"syscall"
	"time"

	"github.com/phpsandbox/rook/internal/agent"
	"github.com/phpsandbox/rook/internal/host"
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

	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg agent.Config) error {
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

	fmt.Println("connected, waiting for commands...")

	go runRelayLoop(ctx, dataWS, relay)

	for {
		msg, err := controlWS.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Fprintf(os.Stderr, "read error: %v, reconnecting...\n", err)
			if err := controlWS.ConnectWithRetry(ctx); err != nil {
				return err
			}
			_ = sendHello(ctx, controlWS, cfg.ServerID, state.IDs())
			continue
		}

		go handleCommand(ctx, msg, controlWS, executor)
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

func runRelayLoop(ctx context.Context, ws *agent.WSClient, relay *agent.RelayManager) {
	defer relay.Reset(context.Canceled)
	for {
		frame, err := ws.ReadRelayFrame(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			relay.Reset(fmt.Errorf("data plane disconnected: %w", err))
			fmt.Fprintf(os.Stderr, "data read error: %v, reconnecting...\n", err)
			if err := ws.ConnectWithRetry(ctx); err != nil {
				return
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
