package syncer

import (
	"bytes"
	"fmt"
	"sort"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// Remove only scanner-proven native entries before generating the managed
// block. The parser identifies quoted/dotted keys and multiline values; byte
// ranges keep unrelated settings, formatting and comments intact.
func removeAdoptedTOML(before []byte, proofs map[string]MCPAdoption) ([]byte, error) {
	bom := []byte{0xef, 0xbb, 0xbf}
	data := bytes.TrimPrefix(before, bom)
	var original map[string]any
	if err := toml.Unmarshal(data, &original); err != nil {
		return nil, err
	}
	entries, _ := original["mcp_servers"].(map[string]any)
	remaining := map[string]any{}
	for name, entry := range entries {
		if _, adopted := proofs[name]; !adopted {
			remaining[name] = entry
		}
	}
	type span struct{ start, end int }
	var removals []span
	var scope []string
	inlineRoot := false
	parser := unstable.Parser{KeepComments: true}
	parser.Reset(data)
	pending := -1
	for parser.NextExpression() {
		node := parser.Expression()
		position := int(node.Raw.Offset)
		if node.Kind == unstable.Table || node.Kind == unstable.ArrayTable || node.Kind == unstable.KeyValue {
			key := node.Key()
			if !key.Next() {
				return nil, fmt.Errorf("cannot locate native TOML MCP entry")
			}
			position = int(key.Node().Raw.Offset)
		}
		start := bytes.LastIndexByte(data[:position], '\n') + 1
		// Expression boundaries include closing brackets of multiline arrays.
		// Comments are separate boundaries and remain byte-for-byte intact.
		if pending >= 0 {
			removals[pending].end = start
			pending = -1
		}
		if node.Kind != unstable.Table && node.Kind != unstable.ArrayTable && node.Kind != unstable.KeyValue {
			continue
		}
		var keys []string
		key := node.Key()
		for key.Next() {
			keys = append(keys, string(key.Node().Data))
		}
		path := keys
		if node.Kind == unstable.KeyValue {
			path = append(append([]string(nil), scope...), keys...)
		} else {
			scope = keys
		}
		remove := len(path) >= 2 && path[0] == "mcp_servers" && proofs[path[1]].Name != ""
		if node.Kind == unstable.KeyValue && len(path) == 1 && path[0] == "mcp_servers" {
			// Inline root tables are sealed by TOML. Move remaining MCP entries
			// to ordinary tables at EOF so the managed siblings can be added.
			remove, inlineRoot = true, true
		}
		if !remove {
			continue
		}
		removals = append(removals, span{start, len(data)})
		pending = len(removals) - 1
	}
	if err := parser.Error(); err != nil {
		return nil, err
	}
	sort.Slice(removals, func(i, j int) bool { return removals[i].start < removals[j].start })
	var after []byte
	offset := 0
	for _, removal := range removals {
		if removal.start < offset {
			return nil, fmt.Errorf("overlapping native TOML MCP entries")
		}
		after = append(after, data[offset:removal.start]...)
		offset = removal.end
	}
	after = append(after, data[offset:]...)
	if inlineRoot && len(remaining) > 0 {
		encoded, err := toml.Marshal(map[string]any{"mcp_servers": remaining})
		if err != nil {
			return nil, err
		}
		if len(after) > 0 && after[len(after)-1] != '\n' {
			after = append(after, '\n')
		}
		after = append(after, encoded...)
	}
	var checked map[string]any
	if err := toml.Unmarshal(after, &checked); err != nil {
		return nil, fmt.Errorf("invalid TOML after native adoption: %w", err)
	}
	if checked == nil {
		checked = map[string]any{}
	}
	if len(remaining) > 0 {
		original["mcp_servers"] = remaining
	} else {
		delete(original, "mcp_servers")
	}
	if current, ok := checked["mcp_servers"].(map[string]any); ok && len(current) == 0 {
		delete(checked, "mcp_servers")
	}
	if digest(original) != digest(checked) {
		return nil, fmt.Errorf("native TOML adoption would change unrelated settings")
	}
	if len(data) != len(before) {
		after = append(append([]byte(nil), bom...), after...)
	}
	return after, nil
}
