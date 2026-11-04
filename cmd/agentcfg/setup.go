package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"agentcfg/internal/pathutil"
	"agentcfg/internal/syncer"
	"agentcfg/internal/tui"
)

func initialized(home string) (bool, error) {
	if info, err := os.Lstat(filepath.Join(home, ".agentcfg")); err == nil {
		if !info.IsDir() || pathutil.IsRedirected(info) {
			return false, errors.New("canonical configuration must be a real user-level directory")
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	for _, name := range []string{"agentcfg.json", "mcp.json"} {
		info, err := os.Lstat(filepath.Join(home, ".agentcfg", name))
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.Mode().IsRegular() || pathutil.IsRedirected(info) {
			return false, errors.New("canonical configuration must contain regular user-level files")
		}
	}
	return true, nil
}

func configure(ctx context.Context, home string, input io.Reader, output io.Writer) error {
	ready, err := initialized(home)
	if err != nil {
		return err
	}
	if !ready {
		description := ""
		var partial *syncer.RestorePlan
		if _, err := os.Lstat(filepath.Join(home, ".agentcfg")); err == nil {
			partial, err = syncer.BuildRestorePlan(home)
			if err != nil {
				return fmt.Errorf("incomplete canonical configuration; review agentcfg restore --plain before initializing again: %w", err)
			}
			if len(partial.Changes) != 0 {
				return errors.New("incomplete canonical configuration with native changes; review agentcfg restore --plain before initializing again")
			}
			description = "Incomplete canonical sources.\nArchive: " + partial.Archive
		}
		confirmed, err := terminalConfirm(ctx, input, output, "Initialize agentcfg first?", description)
		if err != nil || !confirmed {
			return err
		}
		if partial != nil {
			if !terminalPresentation(input, output, false) {
				_, err := fmt.Fprintln(output, "Run agentcfg restore in an interactive terminal (or restore --yes), then agentcfg init. No files changed.")
				return err
			}
			if err := partial.Apply(); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(output, "Archived incomplete configuration:", partial.Archive); err != nil {
				return err
			}
		}
		flags := flag.NewFlagSet("init", flag.ContinueOnError)
		flags.SetOutput(output)
		if err := initialize(ctx, flags, &home, nil, input, output); err != nil {
			return err
		}
		ready, err = initialized(home)
		if err != nil || !ready {
			return err
		}
	}

	return tui.RunMenu(ctx, home, input, output)
}
