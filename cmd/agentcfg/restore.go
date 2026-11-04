package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"agentcfg/internal/syncer"
)

func restore(ctx context.Context, flags *flag.FlagSet, home string, args []string, input io.Reader, output io.Writer) error {
	plain := flags.Bool("plain", false, "preview restoration without writing")
	yes := flags.Bool("yes", false, "explicitly approve restoring native settings and archiving canonical sources")
	if err := parse(flags, args); err != nil {
		return helpResult(err)
	}
	if *plain && *yes {
		return errors.New("restore --plain cannot be combined with --yes")
	}
	plan, err := syncer.BuildRestorePlan(home)
	if err != nil {
		return err
	}
	var preview strings.Builder
	fmt.Fprintf(&preview, "Restore %d native change(s) to the pre-init state:\n", len(plan.Changes))
	for _, change := range plan.Changes {
		fmt.Fprintf(&preview, "- %s: %s\n", change.Action, change.Path)
	}
	fmt.Fprintf(&preview, "Archive all canonical sources and backups at: %s\n", plan.Archive)
	if _, err := io.WriteString(output, preview.String()); err != nil {
		return err
	}
	if *plain {
		return nil
	}
	if !*yes {
		if !terminalPresentation(input, output, false) {
			_, err := fmt.Fprintln(output, "Preview only; run restore in an interactive terminal or use --yes to apply explicitly.")
			return err
		}
		confirmed, err := terminalConfirm(ctx, input, output, "Restore the pre-init configuration?", "")
		if err != nil || !confirmed {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := plan.Apply(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Restored pre-init configuration. Archived agentcfg data: %s\nRun agentcfg init to initialize again.\n", plan.Archive)
	return err
}
