package main

import "testing"

func TestAgentPlaneURLUsesWebSocketScheme(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		controlPlane string
		expected     string
	}{
		"HTTP":  {controlPlane: "http://rook.phpsandbox.test/connect", expected: "ws://rook.phpsandbox.test/connect?channel=control&server_id=server-1"},
		"HTTPS": {controlPlane: "https://rook.phpsandbox.io/connect", expected: "wss://rook.phpsandbox.io/connect?channel=control&server_id=server-1"},
		"WS":    {controlPlane: "ws://rook.phpsandbox.test/connect", expected: "ws://rook.phpsandbox.test/connect?channel=control&server_id=server-1"},
		"WSS":   {controlPlane: "wss://rook.phpsandbox.io/connect", expected: "wss://rook.phpsandbox.io/connect?channel=control&server_id=server-1"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			actual := agentPlaneURL(test.controlPlane, "server-1", "control")
			if actual != test.expected {
				t.Fatalf("agentPlaneURL() = %q, want %q", actual, test.expected)
			}
		})
	}
}
