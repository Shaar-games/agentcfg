package tui

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agentcfg/internal/auth"
)

func authMenuFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("AGENTCFG_ACCESSIBLE", "1")
	t.Setenv("AGENTCFG_OAUTH_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x47}, 32)))
	root := settingsFixture(t)
	data := `{"mcpServers":{
		"alpha":{"command":"inert-fixture"},
		"docs":{"url":"https://public.example.invalid/mcp"},
		"private":{"url":"https://private.example.invalid/mcp","oauth":{"clientId":"fixture-client","redirectUrl":"http://127.0.0.1:58472/callback"}}
	}}`
	if err := os.WriteFile(filepath.Join(root, ".agentcfg", "mcp.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestAuthenticationIsVisibleInMenusWithoutWriting(t *testing.T) {
	for _, test := range []struct {
		name, answers string
		run           func(context.Context, string, io.Reader, io.Writer) error
	}{
		{"inventory", "2\n3\n4\n6\n", RunList},
		{"configuration", "2\n3\n3\n4\n6\n", RunMenu},
		{"mcp-category", "3\n3\n4\n", RunMCP},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := authMenuFixture(t)
			before := compactMenuSnapshot(t, root)
			var output bytes.Buffer
			if err := test.run(context.Background(), root, terminalByteReader{strings.NewReader(test.answers)}, &output); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"login required", "auth unknown", "Authentication: login required", "Run: agentcfg auth login private"} {
				if !strings.Contains(output.String(), want) {
					t.Errorf("missing actionable authentication state %q: %s", want, output.String())
				}
			}
			if test.name != "mcp-category" && !strings.Contains(output.String(), "1 MCP requires login") {
				t.Errorf("overview hid a known login requirement: %s", output.String())
			}
			if test.name != "inventory" {
				command := strings.Index(output.String(), "Run: agentcfg auth login private")
				details := strings.Index(output.String(), "Show transport and routing details")
				if command < 0 || command > details {
					t.Errorf("MCP configuration hid the login command until opening details: %s", output.String())
				}
			}
			if strings.Contains(output.String(), "agentcfg auth login docs") || strings.Contains(output.String(), "agentcfg auth login alpha") {
				t.Errorf("menu demanded login for an unknown/public or stdio server: %s", output.String())
			}
			if !reflect.DeepEqual(before, compactMenuSnapshot(t, root)) {
				t.Fatal("browsing authentication changed configuration, credentials, locks or backups")
			}
		})
	}
}

func TestAuthenticationStorageErrorStaysVisibleWithoutCredentialText(t *testing.T) {
	root := authMenuFixture(t)
	const secret = "INVALID-KEY-MUST-NOT-BE-PRINTED"
	t.Setenv("AGENTCFG_OAUTH_KEY", secret)
	before := compactMenuSnapshot(t, root)
	var output bytes.Buffer
	if err := RunMCP(context.Background(), root, terminalByteReader{strings.NewReader("3\n3\n4\n")}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "credential status unavailable") || !strings.Contains(output.String(), "Check the encryption key") {
		t.Fatalf("storage error was hidden or misreported: %s", output.String())
	}
	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), "agentcfg auth login private") {
		t.Fatalf("storage error leaked a key or incorrectly requested a fresh login: %s", output.String())
	}
	if !reflect.DeepEqual(before, compactMenuSnapshot(t, root)) {
		t.Fatal("failed credential inspection changed the user fixture")
	}
}

func TestAuthenticationNoticeDoesNotTreatRefreshOrUnknownAsLogin(t *testing.T) {
	summaries := map[string]auth.Summary{
		"unknown": {State: auth.SummaryUnknown},
		"refresh": {State: auth.SummaryRefreshAvailable},
		"stored":  {State: auth.SummaryCredentialsSaved},
		"broken":  {State: auth.SummaryStoreError},
	}
	if got := authenticationNotice(summaries); got != "" {
		t.Fatalf("overview demanded a login without local evidence: %q", got)
	}
	summaries["first"] = auth.Summary{State: auth.SummaryLoginRequired}
	summaries["second"] = auth.Summary{State: auth.SummaryLoginRequired}
	if got := authenticationNotice(summaries); !strings.HasPrefix(got, "2 MCPs require login.") {
		t.Fatalf("overview did not count the known requirements: %q", got)
	}
}
