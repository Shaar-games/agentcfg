package tui

import (
	"fmt"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"
)

func (s interfaceSession) authenticationSummaries(cfg *config.Config) (map[string]auth.Summary, error) {
	summaries := make(map[string]auth.Summary, len(cfg.Servers))
	for name, server := range cfg.Servers {
		server.Name = name
		summary, err := auth.InspectSummary(s.ctx, server, auth.Options{Home: s.root})
		if err != nil {
			return nil, err
		}
		summaries[name] = summary
	}
	return summaries, nil
}

func authenticationNotice(summaries map[string]auth.Summary) string {
	required := 0
	for _, summary := range summaries {
		if summary.State == auth.SummaryLoginRequired {
			required++
		}
	}
	if required == 0 {
		return ""
	}
	if required == 1 {
		return "1 MCP requires login. Open MCP servers for its login command."
	}
	return fmt.Sprintf("%d MCPs require login. Open MCP servers for their login commands.", required)
}

func authenticationDetails(summary auth.Summary) string {
	if summary.State == auth.SummaryNotManaged || summary.Label == "" {
		return ""
	}
	text := fmt.Sprintf("Authentication: %s\n%s", summary.Label, summary.Message)
	if summary.LoginCommand != "" {
		text += "\nRun: " + summary.LoginCommand
	}
	return text
}
