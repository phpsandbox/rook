package main

import (
	"context"
	"encoding/json"
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
	configPath := flag.String("config", agent.DefaultConfigPath, "path to rook config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

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
	proxy := agent.NewProxy(state)
	controlWS := agent.NewWSClient(agentPlaneURL(cfg.ControlPlane, cfg.ServerID, "control"), cfg.Token)
	dataWS := agent.NewWSClient(agentPlaneURL(cfg.ControlPlane, cfg.ServerID, "data"), cfg.Token)
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
		Capabilities: []string{host.Capability},
		Deployments:  deployments,
	})
}

func runRelayLoop(ctx context.Context, ws *agent.WSClient, relay *agent.RelayManager) {
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
		result, err = executor.Execute(ctx, request, func(output string) { send(agent.OutboundMessage{Type: "log", Stream: "host", Content: output}) })
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
