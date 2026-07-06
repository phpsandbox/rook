package agent

import (
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

func TestInboundMessageDecodePayloadFromMessagePackPayload(t *testing.T) {
	raw, err := msgpack.Marshal(DeployPayload{
		DeploymentID: "dep-1",
		Source: SourceRef{
			Provider: SourceProviderPath,
			Path:     "/tmp/source",
		},
		Manifest: DeployManifest{
			SchemaVersion: 1,
			Build: DeployManifestBuild{
				KeepWorkspace: true,
			},
		},
		Plan: Plan{
			Strategy: "laravel",
			Build: BuildPlan{
				Image: "phpsandbox/php:latest",
			},
			Runtime: RuntimePlan{
				Port:       8000,
				HealthPath: "/",
			},
		},
		Bundle: &DeployBundle{
			Format: "tar.gz",
			Size:   3,
			SHA256: "sha256:test",
			Data:   []byte("abc"),
		},
		Env: map[string]string{
			"APP_ENV": "production",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	payload, err := (InboundMessage{messagePackPayload: raw}).DecodeDeployPayload()
	if err != nil {
		t.Fatal(err)
	}

	if payload.DeploymentID != "dep-1" {
		t.Fatalf("deployment id = %q", payload.DeploymentID)
	}
	if payload.Source.Path != "/tmp/source" {
		t.Fatalf("source = %#v", payload.Source)
	}
	if payload.Manifest.SchemaVersion != 1 || !payload.Manifest.Build.KeepWorkspace {
		t.Fatalf("manifest = %#v", payload.Manifest)
	}
	if payload.Plan.Runtime.Port != 8000 {
		t.Fatalf("runtime port = %d", payload.Plan.Runtime.Port)
	}
	if payload.Plan.Runtime.HealthPath != "/" {
		t.Fatalf("runtime health path = %q", payload.Plan.Runtime.HealthPath)
	}
	if payload.Plan.Build.Image != "phpsandbox/php:latest" {
		t.Fatalf("build image = %q", payload.Plan.Build.Image)
	}
	if len(payload.Plan.Runtime.Command) != 0 {
		t.Fatalf("runtime command = %#v", payload.Plan.Runtime.Command)
	}
	if payload.Bundle == nil || string(payload.Bundle.Data) != "abc" {
		t.Fatalf("bundle = %#v", payload.Bundle)
	}
	if payload.Env["APP_ENV"] != "production" {
		t.Fatalf("env = %#v", payload.Env)
	}
}
