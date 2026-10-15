package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestConfigRequiresSecureControlPlane(t *testing.T) {
	for _, test := range []struct {
		url             string
		insecure, valid bool
	}{
		{"https://control.example/connect", false, true},
		{"wss://control.example/connect", false, true},
		{"http://127.0.0.1:8080/connect", false, false},
		{"ws://127.0.0.1:8080/connect", false, false},
		{"http://127.0.0.1:8080/connect", true, true},
		{"ws://127.0.0.1:8080/connect", true, true},
		{"https://user:password@control.example/connect", false, false},
		{"https:///connect", false, false},
		{"https://control.example:invalid/connect", false, false},
		{"https://control.example/connect#fragment", false, false},
		{"ftp://control.example/connect", true, false},
	} {
		t.Run(test.url+strconv.FormatBool(test.insecure), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rook.yaml")
			data := "server_id: server\ntoken: secret\ncontrol_plane: " + strconv.Quote(test.url) + "\nallow_insecure_control_plane: " + strconv.FormatBool(test.insecure) + "\n"
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if (err == nil) != test.valid {
				t.Fatalf("LoadConfig error=%v, valid=%v", err, test.valid)
			}
		})
	}
}
