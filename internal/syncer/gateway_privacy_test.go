package syncer

import (
	"encoding/json"
	"strings"
	"testing"
)

func gatewayPrivacySamples() map[string][]byte {
	resource := "http://127.0.0.1:43877/mcp/" + strings.Repeat("A", 43) + "/cmVtb3Rl/Y3Vyc29y"
	encoded, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"remote": map[string]any{"url": resource}}})
	escaped := strings.ReplaceAll(string(encoded), "/", `\u002f`)
	return map[string][]byte{
		"json":                  encoded,
		"json escaped slash":    []byte(strings.ReplaceAll(string(encoded), "/", `\/`)),
		"json unicode slash":    []byte(escaped),
		"json unicode token":    []byte(strings.ReplaceAll(escaped, "A", `\u0041`)),
		"jsonc BOM":             []byte("\xef\xbb\xbf// client formatting\n" + strings.TrimSuffix(escaped, "}") + ",}\n"),
		"toml":                  []byte("[mcp_servers.remote]\nurl = '" + resource + "'\n"),
		"toml escaped BOM":      []byte("\xef\xbb\xbf[mcp_servers.remote]\nurl = \"" + strings.ReplaceAll(resource, "/", `\u002f`) + "\"\n"),
		"toml continued string": []byte("[mcp_servers.remote]\nurl = \"\"\"" + strings.Replace(resource, strings.Repeat("A", 43), strings.Repeat("A", 20)+"\\\n  "+strings.Repeat("A", 23), 1) + "\"\"\"\n"),
	}
}

func TestGatewayCapabilityDetectionPreservesNativeSemantics(t *testing.T) {
	for name, data := range gatewayPrivacySamples() {
		t.Run(name, func(t *testing.T) {
			if !containsGatewayCapability(data) {
				t.Fatal("native capability was not recognized after format decoding")
			}
			journal, err := json.Marshal(initRestorePoint{Version: 1, Native: []restoreEntry{{Before: data}}})
			if err != nil {
				t.Fatal(err)
			}
			if !containsGatewayCapability(journal) {
				t.Fatal("base64-encoded native capability was not recognized in the restore journal")
			}
		})
	}
	for _, data := range []string{"", "ordinary instructions", `{"url":"http://127.0.0.1:12345/mcp"}`, `{"url":"http:\/\/127.0.0.1:12345\/mcp\/ordinary\/server"}`, `url = "https://example.test/mcp/public"`} {
		if containsGatewayCapability([]byte(data)) {
			t.Fatal("ordinary content was classified as a gateway capability")
		}
	}
}
