package syncer

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agentcfg/internal/config"
	"gopkg.in/yaml.v3"
)

const maxInstructionsBytes = 1 << 20

// Split routing metadata from AGENTS.md. Copy the Markdown body unchanged.
func instructionsContent(original []byte) ([]byte, []string, error) {
	if len(original) > maxInstructionsBytes {
		return nil, nil, errors.New("global AGENTS.md exceeds 1 MiB")
	}
	input := bytes.TrimPrefix(original, []byte{0xef, 0xbb, 0xbf})
	if !bytes.HasPrefix(input, []byte("---\n")) && !bytes.HasPrefix(input, []byte("---\r\n")) {
		return append([]byte(nil), original...), nil, nil
	}
	first := bytes.IndexByte(input, '\n') + 1
	if first <= 0 {
		return nil, nil, errors.New("invalid AGENTS.md opening delimiter")
	}
	remaining := input[first:]
	pos := 0
	var header, body []byte
	found := false
	closingStart := 0
	closingEnd := 0
	for pos < len(remaining) {
		length := bytes.IndexByte(remaining[pos:], '\n')
		if length < 0 {
			break
		}
		last := pos + length + 1
		line := bytes.TrimSuffix(remaining[pos:last-1], []byte("\r"))
		if bytes.Equal(line, []byte("---")) {
			closingStart = pos
			header = remaining[:pos]
			body = remaining[last:]
			closingEnd = last
			found = true
			break
		}
		pos = last
		if pos > 64<<10 {
			return nil, nil, errors.New("AGENTS.md frontmatter exceeds 64 KiB")
		}
	}
	if !found {
		return nil, nil, errors.New("AGENTS.md frontmatter has no closing delimiter")
	}
	var document yaml.Node
	if err := yaml.Unmarshal(header, &document); err != nil {
		return nil, nil, fmt.Errorf("AGENTS.md YAML: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("AGENTS.md frontmatter must be a mapping")
	}
	fields := document.Content[0]
	var harnesses []string
	metadataLine := 0
	harnessLine := 0
	metadataOnlyHarness := false
	seenTop := map[string]bool{}
	for i := 0; i < len(fields.Content); i += 2 {
		key := fields.Content[i].Value
		if seenTop[key] {
			return nil, nil, fmt.Errorf("duplicate AGENTS.md key %q", key)
		}
		seenTop[key] = true
		if key != "metadata" {
			continue
		}
		metadata := fields.Content[i+1]
		metadataLine = fields.Content[i].Line
		if metadata.Style&yaml.FlowStyle != 0 {
			return nil, nil, errors.New("AGENTS.md metadata must use block YAML syntax")
		}
		if metadata.Kind != yaml.MappingNode {
			return nil, nil, errors.New("AGENTS.md metadata must be a map")
		}
		seenKeys := map[string]bool{}
		for j := 0; j < len(metadata.Content); j += 2 {
			name, value := metadata.Content[j], metadata.Content[j+1]
			if seenKeys[name.Value] {
				return nil, nil, fmt.Errorf("duplicate AGENTS.md metadata key %q", name.Value)
			}
			seenKeys[name.Value] = true
			if name.Value != "harness" {
				continue
			}
			harnessLine = name.Line
			metadataOnlyHarness = len(metadata.Content) == 2
			if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
				return nil, nil, errors.New("AGENTS.md metadata.harness must be a comma-separated string")
			}
			if value.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 || strings.ContainsRune(value.Value, '\n') {
				return nil, nil, errors.New("AGENTS.md metadata.harness must use a single-line YAML scalar")
			}
			if strings.TrimSpace(value.Value) == "none" {
				harnesses = []string{"none"}
				continue
			}
			allowed := map[string]bool{"claude-code": true, "codex": true, "opencode": true, "cursor": true, "gemini": true, "antigravity": true, "vscode": true}
			unique := map[string]bool{}
			for _, segment := range strings.Split(value.Value, ",") {
				name := strings.TrimSpace(segment)
				if !allowed[name] {
					return nil, nil, fmt.Errorf("invalid AGENTS.md metadata.harness %q", name)
				}
				if !unique[name] {
					harnesses = append(harnesses, name)
					unique[name] = true
				}
			}
		}
	}
	if harnessLine == 0 {
		// No routing metadata was present. Preserve unrelated frontmatter
		// instead of deleting configuration that belongs to another client.
		return append([]byte(nil), original...), harnesses, nil
	}
	if metadataOnlyHarness && len(fields.Content) == 2 {
		// The only frontmatter information was routing metadata. An AGENTS.md
		// destination should receive its Markdown body with no agentcfg header.
		return append([]byte(nil), body...), harnesses, nil
	}
	lines := bytes.SplitAfter(header, []byte{'\n'})
	removed := map[int]bool{harnessLine: true}
	if metadataOnlyHarness {
		removed[metadataLine] = true
	}
	var rewritten bytes.Buffer
	rewritten.Write(original[:len(original)-len(input)]) // preserve UTF-8 BOM
	rewritten.Write(input[:first])
	for i, line := range lines {
		if !removed[i+1] {
			rewritten.Write(line)
		}
	}
	rewritten.Write(remaining[closingStart:closingEnd])
	rewritten.Write(body)
	return rewritten.Bytes(), harnesses, nil
}

func (p *Plan) planInstructions(c *config.Config, opts Options, selected map[string]bool, old manifest) error {
	if opts.Scope != "user" {
		return nil
	}
	source := filepath.Join(c.Root, ".agentcfg", "AGENTS.md")
	if err := ensureNoSymlinkParents(c.Root, source); err != nil {
		return err
	}
	contents, exists, err := readOptional(source)
	if err != nil {
		return err
	}
	var desired []byte
	var filter []string
	if exists {
		info, err := os.Lstat(source)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("AGENTS.md source must be regular: %s", source)
		}
		desired, filter, err = instructionsContent(contents)
		if err != nil {
			return err
		}
	}
	destinations := map[string]string{
		"claude-code": filepath.Join(opts.Home, ".claude", "CLAUDE.md"),
		"codex":       filepath.Join(opts.Home, ".codex", "AGENTS.md"),
		"opencode":    filepath.Join(opts.Home, ".config", "opencode", "AGENTS.md"),
	}
	paths := map[string]bool{}
	for _, path := range destinations {
		paths[path] = true
	}
	for path := range old.Instructions {
		if !paths[path] {
			return fmt.Errorf("unexpected managed global instruction path %s", path)
		}
	}
	for harness, path := range destinations {
		previous := old.Instructions[path]
		want := exists && selected[harness] && config.Allows(filter, harness)
		if !want && previous == "" {
			// A never-managed instruction target is outside this sync's write
			// set, just like an unselected native MCP or skills destination.
			continue
		}
		if err := ensureNoSymlinkParents(opts.Home, path); err != nil {
			return err
		}
		original, present, err := readOptional(path)
		if err != nil {
			return err
		}
		if previous != "" && (!present || rawDigest(original) != previous) {
			return fmt.Errorf("managed global instructions modified manually: %s", path)
		}
		if !want {
			if previous != "" {
				p.Changes = append(p.Changes, Change{Path: path, Action: "remove managed global instructions", before: original, exists: present, remove: true, source: source, sourceExists: exists, sourceHash: rawDigest(contents)})
			}
			continue
		}
		if present && previous == "" {
			return fmt.Errorf("global instructions exist and are not owned by agentcfg: %s", path)
		}
		p.state.Instructions[path] = rawDigest(desired)
		if !bytes.Equal(original, desired) {
			p.Changes = append(p.Changes, Change{Path: path, Action: "sync global instructions", before: original, after: desired, exists: present, source: source, sourceExists: true, sourceHash: rawDigest(contents)})
		}
	}
	if exists {
		for _, harness := range c.Settings.Harnesses {
			if _, supported := destinations[harness]; !supported && config.Allows(filter, harness) {
				p.Warnings = append(p.Warnings, fmt.Sprintf("%s has no verified user-level AGENTS.md adapter; instructions not installed there", harness))
			}
		}
	}
	return nil
}
