package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
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
	ws := agent.NewWSClient(agentControlPlaneURL(cfg.ControlPlane, cfg.ServerID), cfg.Token)

	fmt.Printf("rook %s connecting to %s (server: %s)\n", version, cfg.ControlPlane, cfg.ServerID)

	if err := ws.ConnectWithRetry(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer ws.Close()

	if err := sendHello(ctx, ws, cfg.ServerID, state.DeploymentIDs()); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	fmt.Println("connected, waiting for commands...")

	for {
		msg, err := ws.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Fprintf(os.Stderr, "read error: %v, reconnecting...\n", err)
			if err := ws.ConnectWithRetry(ctx); err != nil {
				return err
			}
			_ = sendHello(ctx, ws, cfg.ServerID, state.DeploymentIDs())
			continue
		}

		go handleCommand(ctx, msg, ws, deployer, proxy)
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

func agentControlPlaneURL(rawURL string, serverID string) string {
	if strings.Contains(rawURL, "server_id=") {
		return rawURL
	}
	separator := "?"
	if strings.Contains(rawURL, "?") {
		separator = "&"
	}
	return rawURL + separator + "server_id=" + url.QueryEscape(serverID)
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

	case "http":
		var payload agent.HTTPRequestPayload
		if err := msg.DecodePayload(&payload); err != nil {
			send(agent.OutboundMessage{Type: "result", Success: false, Error: err.Error()})
			return
		}
		status, headers, body, err := proxy.HandleHTTP(payload.DeploymentID, payload.Method, payload.Path, payload.Headers, payload.Body)
		if err != nil {
			send(agent.OutboundMessage{Type: "result", Success: false, Error: err.Error()})
			return
		}
		send(agent.OutboundMessage{
			Type:      "http_response",
			RequestID: payload.RequestID,
			Status:    status,
			Headers:   headers,
			Body:      body,
		})

	default:
		send(agent.OutboundMessage{Type: "result", Success: false, Error: fmt.Sprintf("unsupported command %q", msg.Type)})
	}
}
