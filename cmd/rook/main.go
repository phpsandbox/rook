package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/phpsandbox/rook/internal/agent"
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
	docker := agent.NewDockerManager()
	if err := docker.Available(); err != nil {
		return fmt.Errorf("prerequisite check failed: %w", err)
	}

	state := agent.NewStateStore(cfg.StateDir)
	if err := state.Load(); err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	deployer := agent.NewDeployer(docker, state)
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

	if err := sendHello(ctx, controlWS, cfg.ServerID, state.DeploymentIDs()); err != nil {
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
			_ = sendHello(ctx, controlWS, cfg.ServerID, state.DeploymentIDs())
			continue
		}

		go handleCommand(ctx, msg, controlWS, deployer, proxy)
	}
}

func sendHello(ctx context.Context, ws *agent.WSClient, serverID string, deployments []string) error {
	return ws.Send(ctx, agent.OutboundMessage{
		Type:        "hello",
		ServerID:    serverID,
		Version:     version,
		Deployments: deployments,
	})
}

func runRelayLoop(ctx context.Context, ws *agent.WSClient, relay *agent.RelayManager) {
	for {
		var frame agent.RelayFrame
		if err := ws.ReadMessagePack(ctx, &frame); err != nil {
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
	query := parsed.Query()
	if query.Get("server_id") == "" {
		query.Set("server_id", serverID)
	}
	query.Set("channel", channel)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func handleCommand(ctx context.Context, msg agent.InboundMessage, ws *agent.WSClient, deployer *agent.Deployer, proxy *agent.Proxy) {
	send := func(out agent.OutboundMessage) {
		out.CommandID = msg.CommandID
		_ = ws.Send(ctx, out)
	}

	switch msg.Type {
	case "deploy":
		var payload agent.DeployPayload
		if err := msg.DecodePayload(&payload); err != nil {
			send(agent.OutboundMessage{Type: "result", Success: false, Error: err.Error()})
			return
		}
		if err := deployer.Deploy(ctx, payload, send); err != nil {
			send(agent.OutboundMessage{Type: "result", Success: false, Error: err.Error()})
			return
		}
		send(agent.OutboundMessage{Type: "result", Success: true})

	case "stop":
		var payload agent.StopPayload
		if err := msg.DecodePayload(&payload); err != nil {
			send(agent.OutboundMessage{Type: "result", Success: false, Error: err.Error()})
			return
		}
		if err := deployer.Stop(ctx, payload.DeploymentID); err != nil {
			send(agent.OutboundMessage{Type: "result", Success: false, Error: err.Error()})
			return
		}
		send(agent.OutboundMessage{Type: "result", Success: true})

	case "delete":
		var payload agent.DeletePayload
		if err := msg.DecodePayload(&payload); err != nil {
			send(agent.OutboundMessage{Type: "result", Success: false, Error: err.Error()})
			return
		}
		if err := deployer.Delete(ctx, payload.DeploymentID); err != nil {
			send(agent.OutboundMessage{Type: "result", Success: false, Error: err.Error()})
			return
		}
		send(agent.OutboundMessage{Type: "result", Success: true})

	case "logs.tail":
		var payload agent.LogsTailPayload
		if err := msg.DecodePayload(&payload); err != nil {
			send(agent.OutboundMessage{Type: "result", Success: false, Error: err.Error()})
			return
		}
		content, err := deployer.TailLogs(ctx, payload.DeploymentID, payload.Lines)
		if err != nil {
			send(agent.OutboundMessage{Type: "result", Success: false, Error: err.Error()})
			return
		}
		send(agent.OutboundMessage{Type: "result", Success: true, Content: content})

	default:
		send(agent.OutboundMessage{Type: "result", Success: false, Error: fmt.Sprintf("unsupported command %q", msg.Type)})
	}
}
