package agent

import "testing"

func TestInboundMessageDecodePayloadFromMessagePackDecodedMap(t *testing.T) {
	raw := map[string]any{
		"deploymentId": "dep-1",
		"source": map[string]any{
			"provider": "path",
			"path":     "/tmp/source",
		},
		"plan": map[string]any{
			"strategy": "laravel",
			"runtime": map[string]any{
				"port":    8000,
				"command": []any{"/usr/local/bin/okra-laravel-start"},
			},
		},
		"bundle": map[string]any{
			"format": "tar.gz",
			"size":   int8(3),
			"sha256": "sha256:test",
			"data":   []byte("abc"),
		},
		"env": map[string]any{
			"APP_ENV": "production",
		},
	}

	var payload DeployPayload
	if err := (InboundMessage{decoded: raw}).DecodePayload(&payload); err != nil {
		t.Fatal(err)
	}

	if payload.DeploymentID != "dep-1" {
		t.Fatalf("deployment id = %q", payload.DeploymentID)
	}
	if payload.Source.Path != "/tmp/source" {
		t.Fatalf("source = %#v", payload.Source)
	}
	if payload.Plan.Runtime.Port != 8000 {
		t.Fatalf("runtime port = %d", payload.Plan.Runtime.Port)
	}
	if payload.Bundle == nil || string(payload.Bundle.Data) != "abc" {
		t.Fatalf("bundle = %#v", payload.Bundle)
	}
	if payload.Env["APP_ENV"] != "production" {
		t.Fatalf("env = %#v", payload.Env)
	}
}
