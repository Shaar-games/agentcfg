package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type failingOutput struct{}

func (failingOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type cancellingOutput struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancellingOutput) Write(p []byte) (int, error) {
	w.cancel()
	return w.Buffer.Write(p)
}

func (w *cancellingOutput) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func TestInitCancellationAndPreviewFailureNeverWrite(t *testing.T) {
	for _, mode := range []string{"already-cancelled", "cancel-during-preview", "broken-output"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var output io.Writer = io.Discard
			want := error(context.Canceled)
			switch mode {
			case "already-cancelled":
				cancel()
			case "cancel-during-preview":
				output = &cancellingOutput{cancel: cancel}
			case "broken-output":
				output, want = failingOutput{}, io.ErrClosedPipe
			}
			err := runInitWizard(ctx, home, initProposal{}, initOptions{Yes: true}, nil, output)
			if !errors.Is(err, want) {
				t.Errorf("initialization error = %v, want %v", err, want)
			}
			entries, err := os.ReadDir(home)
			if err != nil || len(entries) != 0 {
				t.Fatalf("cancelled/unreadable initialization changed home: %v, %v", entries, err)
			}
		})
	}
}

func TestSyncCancellationAndPreviewFailureNeverWrite(t *testing.T) {
	for _, mode := range []string{"already-cancelled", "cancel-during-preview", "broken-output"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			writeScopeFixture(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), `{"harnesses":["cursor"]}`)
			writeScopeFixture(t, filepath.Join(home, ".agentcfg", "mcp.json"), `{"mcpServers":{"sample":{"command":"inert-test-command"}}}`)
			before := projectSnapshot(t, home)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var output io.Writer = io.Discard
			want := error(context.Canceled)
			switch mode {
			case "already-cancelled":
				cancel()
			case "cancel-during-preview":
				output = &cancellingOutput{cancel: cancel}
			case "broken-output":
				output, want = failingOutput{}, io.ErrClosedPipe
			}
			err := run(ctx, []string{"sync", "--apply", "--plain"}, nil, output, io.Discard)
			if !errors.Is(err, want) {
				t.Errorf("synchronization error = %v, want %v", err, want)
			}
			if after := projectSnapshot(t, home); !sameProjectSnapshot(before, after) {
				t.Fatalf("cancelled/unreadable synchronization changed home: before=%v after=%v", before, after)
			}
		})
	}
}

func TestPlainCommandsReportOutputFailures(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	writeScopeFixture(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), `{"harnesses":["cursor"]}`)
	writeScopeFixture(t, filepath.Join(home, ".agentcfg", "mcp.json"), `{"mcpServers":{"sample":{"command":"inert-test-command"}}}`)
	writeScopeFixture(t, filepath.Join(home, ".agentcfg", "skills", "sample", "SKILL.md"), "---\nname: sample\ndescription: Inert test fixture\n---\nDo not execute.\n")
	for _, args := range [][]string{{"list", "--plain"}, {"validate", "--plain"}, {"mcp", "--plain"}, {"skills", "--plain"}, {"harnesses", "--plain"}, {"init", "--plain"}, {"sync", "--plain"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			before := projectSnapshot(t, home)
			err := run(context.Background(), args, nil, failingOutput{}, io.Discard)
			if !errors.Is(err, io.ErrClosedPipe) {
				t.Errorf("output failure was lost: %v", err)
			}
			if after := projectSnapshot(t, home); !sameProjectSnapshot(before, after) {
				t.Fatal("plain command changed configuration")
			}
		})
	}
}

func TestInitWithoutApprovalOnlyPreviews(t *testing.T) {
	home := t.TempDir()
	before := projectSnapshot(t, home)
	err := runInitWizard(context.Background(), home, initProposal{}, initOptions{}, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if after := projectSnapshot(t, home); !sameProjectSnapshot(before, after) {
		t.Fatal("unapproved native init synchronization modified the home")
	}
}
