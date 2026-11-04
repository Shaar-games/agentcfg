package syncer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tailscale/hujson"
)

func pointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func editJSON(before []byte, key string, desired map[string]map[string]any, previous map[string]string) ([]byte, map[string]string, error) {
	return editJSONWithPolicy(before, key, desired, previous, nil)
}

func editJSONWithPolicy(before []byte, key string, desired map[string]map[string]any, previous map[string]string, adopted map[string]MCPAdoption) ([]byte, map[string]string, error) {
	if len(before) != 0 {
		if _, err := uniqueJSONObject(before); err != nil {
			return nil, nil, fmt.Errorf("ambiguous native JSON configuration: %w", err)
		}
	}
	original := before
	bom := []byte{0xef, 0xbb, 0xbf}
	withBOM := bytes.HasPrefix(before, bom)
	if withBOM {
		before = before[len(bom):]
	}
	if len(before) == 0 {
		before = []byte("{}\n")
	}
	value, err := hujson.Parse(append([]byte(nil), before...))
	if err != nil {
		return nil, nil, err
	}
	standard, err := hujson.Standardize(append([]byte(nil), before...))
	if err != nil {
		return nil, nil, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(standard, &document); err != nil || document == nil {
		return nil, nil, fmt.Errorf("configuration must be an object")
	}
	entries := map[string]any{}
	if raw, ok := document[key]; ok {
		if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
			return nil, nil, fmt.Errorf("%s must be an object", key)
		}
	}
	for name, expected := range previous {
		entry, ok := entries[name]
		if !ok || digest(entry) != expected {
			return nil, nil, fmt.Errorf("managed MCP %q was edited or removed manually; resolve the conflict before syncing", name)
		}
	}
	patches := []map[string]any{}
	if raw, exists := document["disabledMcpServers"]; exists && len(adopted) != 0 {
		var disabled []string
		if err := json.Unmarshal(raw, &disabled); err != nil {
			return nil, nil, fmt.Errorf("disabledMcpServers must be a string array")
		}
		retained := []string{}
		for _, name := range disabled {
			if _, imported := adopted[name]; !imported {
				retained = append(retained, name)
			}
		}
		if len(retained) != len(disabled) {
			patches = append(patches, map[string]any{"op": "replace", "path": "/disabledMcpServers", "value": retained})
		}
	}
	if _, ok := document[key]; !ok && len(desired) != 0 {
		patches = append(patches, map[string]any{"op": "add", "path": "/" + pointer(key), "value": map[string]any{}})
	}
	names := map[string]bool{}
	for name := range desired {
		names[name] = true
	}
	for name := range previous {
		names[name] = true
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	owned := map[string]string{}
	for _, name := range ordered {
		wanted, want := desired[name]
		existing, exists := entries[name]
		_, managed := previous[name]
		path := "/" + pointer(key) + "/" + pointer(name)
		if !want {
			patches = append(patches, map[string]any{"op": "remove", "path": path})
			continue
		}
		if exists && !managed {
			return nil, nil, fmt.Errorf("MCP name %q already exists and is not owned by agentcfg", name)
		}
		owned[name] = digest(wanted)
		if exists && digest(existing) == digest(wanted) {
			continue
		}
		patches = append(patches, map[string]any{"op": "add", "path": path, "value": wanted})
	}
	if len(patches) == 0 {
		return original, owned, nil
	}
	encoded, _ := json.Marshal(patches)
	if err := value.Patch(encoded); err != nil {
		return nil, nil, err
	}
	value.Format()
	result := value.Pack()
	if withBOM {
		result = append(append([]byte{}, bom...), result...)
	}
	return result, owned, nil
}
