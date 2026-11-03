package syncer

import (
	"bytes"
	"fmt"

	toml "github.com/pelletier/go-toml/v2"
)

const beginMarker = "# agentcfg:begin managed MCP servers"
const endMarker = "# agentcfg:end managed MCP servers"

func splitBlock(data []byte) ([]byte, []byte, []byte, error) {
	start, end := bytes.Index(data, []byte(beginMarker)), bytes.Index(data, []byte(endMarker))
	if start < 0 && end < 0 {
		return data, nil, nil, nil
	}
	if start < 0 || end < start || bytes.Count(data, []byte(beginMarker)) != 1 || bytes.Count(data, []byte(endMarker)) != 1 {
		return nil, nil, nil, fmt.Errorf("invalid or duplicate agentcfg markers")
	}
	if start > 0 && data[start-1] != '\n' {
		return nil, nil, nil, fmt.Errorf("agentcfg marker must be on its own line")
	}
	end += len(endMarker)
	if end < len(data) && data[end] == '\r' {
		end++
	}
	if end < len(data) && data[end] == '\n' {
		end++
	}
	return data[:start], data[start:end], data[end:], nil
}

func editTOML(before []byte, desired map[string]map[string]any, previous map[string]string, previousBlock string) ([]byte, map[string]string, string, error) {
	original := before
	bom := []byte{0xef, 0xbb, 0xbf}
	withBOM := bytes.HasPrefix(before, bom)
	if withBOM {
		// Windows PowerShell 5.1 writes a UTF-8 BOM for Set-Content.
		// Parse TOML without it, but leave it untouched in generated files.
		before = before[len(bom):]
	}
	var document map[string]any
	if len(before) != 0 {
		if err := toml.Unmarshal(before, &document); err != nil {
			return nil, nil, "", err
		}
	}
	entries, _ := document["mcp_servers"].(map[string]any)
	prefix, block, suffix, err := splitBlock(before)
	if err != nil {
		return nil, nil, "", err
	}
	if len(block) != 0 && (previousBlock == "" || rawDigest(block) != previousBlock) {
		return nil, nil, "", fmt.Errorf("managed TOML block was edited manually or is not owned by agentcfg")
	}
	if previousBlock != "" && len(block) == 0 {
		return nil, nil, "", fmt.Errorf("managed TOML block was removed manually")
	}
	for name, expected := range previous {
		entry, exists := entries[name]
		if !exists || digest(entry) != expected {
			return nil, nil, "", fmt.Errorf("managed MCP %q was modified manually", name)
		}
	}
	owned := map[string]string{}
	for name, wanted := range desired {
		if _, exists := entries[name]; exists && previous[name] == "" {
			return nil, nil, "", fmt.Errorf("MCP name %q already exists and is not owned by agentcfg", name)
		}
		owned[name] = digest(wanted)
	}
	var newBlock []byte
	if len(desired) > 0 {
		encoded, err := toml.Marshal(map[string]any{"mcp_servers": desired})
		if err != nil {
			return nil, nil, "", err
		}
		// Child tables implicitly define mcp_servers. Re-declaring the parent
		// would reject a user's existing explicit [mcp_servers] section.
		encoded = bytes.TrimPrefix(encoded, []byte("[mcp_servers]\n"))
		newBlock = append([]byte(beginMarker+"\n"), encoded...)
		newBlock = append(newBlock, []byte(endMarker+"\n")...)
	}
	if bytes.Equal(block, newBlock) {
		return original, owned, previousBlock, nil
	}
	after := append([]byte(nil), prefix...)
	if len(newBlock) > 0 && len(after) > 0 && after[len(after)-1] != '\n' {
		after = append(after, '\n')
	}
	after = append(after, newBlock...)
	after = append(after, suffix...)
	// Appended tables must not change the meaning of unrelated TOML content.
	var checked map[string]any
	if len(after) != 0 {
		if err := toml.Unmarshal(after, &checked); err != nil {
			return nil, nil, "", fmt.Errorf("generated TOML is invalid: %w", err)
		}
	}
	if withBOM {
		after = append(append([]byte(nil), bom...), after...)
	}
	hash := ""
	if len(newBlock) > 0 {
		hash = rawDigest(newBlock)
	}
	return after, owned, hash, nil
}
