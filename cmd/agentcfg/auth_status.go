package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"
)

type serverAuthentication struct {
	Name string `json:"name"`
	auth.Summary
}

// This is a local snapshot. Merely listing a server must not contact it, create
// credentials, refresh tokens, start the broker or open a browser.
func inspectAuthentication(ctx context.Context, cfg *config.Config) ([]serverAuthentication, error) {
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	var result []serverAuthentication
	for _, name := range names {
		server := cfg.Servers[name]
		server.Name = name
		summary, err := auth.InspectSummary(ctx, server, auth.Options{Home: cfg.Root})
		if err != nil {
			return nil, err
		}
		if summary.State != auth.SummaryNotManaged {
			result = append(result, serverAuthentication{Name: name, Summary: summary})
		}
	}
	return result, nil
}

func authenticationByName(states []serverAuthentication) map[string]auth.Summary {
	result := make(map[string]auth.Summary, len(states))
	for _, state := range states {
		result[state.Name] = state.Summary
	}
	return result
}

func authenticationNotices(states []serverAuthentication) string {
	var text strings.Builder
	for _, state := range states {
		switch state.State {
		case auth.SummaryLoginRequired:
			fmt.Fprintf(&text, "LOGIN REQUIRED: %s\n  %s\n", state.Name, state.LoginCommand)
		case auth.SummaryStoreUnavailable, auth.SummaryStoreError:
			fmt.Fprintf(&text, "AUTHENTICATION STATUS UNAVAILABLE: %s\n  %s\n", state.Name, state.Message)
		}
	}
	return text.String()
}

func writeAuthenticationDetail(output io.Writer, state auth.Summary, indent string) error {
	if state.Label == "" {
		return nil
	}
	if _, err := fmt.Fprintf(output, "%sAuthentication: %s\n", indent, state.Label); err != nil {
		return err
	}
	if state.Message != "" {
		if _, err := fmt.Fprintf(output, "%s%s\n", indent, state.Message); err != nil {
			return err
		}
	}
	if state.LoginCommand != "" {
		_, err := fmt.Fprintf(output, "%sRun: %s\n", indent, state.LoginCommand)
		return err
	}
	return nil
}
