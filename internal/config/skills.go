package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"agentcfg/internal/fsstate"

	"gopkg.in/yaml.v3"
)

const maxFrontmatterBytes = 64 << 10

func (l *loader) loadSkills(location, plugin, boundary string, standalone bool) {
	if _, err := os.Lstat(location); os.IsNotExist(err) {
		return
	}
	path, err := linkedReadPath(boundary, location, false)
	if err != nil {
		l.warn(location, err)
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		l.warn(location, err)
		return
	}
	for _, entry := range entries {
		candidate := filepath.Join(location, entry.Name())
		directory, err := resolveExisting(candidate)
		if err != nil {
			l.warn(candidate, err)
			continue
		}
		info, err := os.Stat(directory)
		if err != nil {
			l.warn(candidate, err)
			continue
		}
		if !info.IsDir() {
			continue
		}
		if standalone && l.generatedSkill(directory) {
			continue
		}
		if _, err = linkedReadPath(boundary, candidate, false); err != nil {
			l.warn(candidate, err)
			continue
		}
		// Preserve the discovered basename for harness installation, including
		// an in-root directory alias whose target has a different basename.
		l.loadSkill(candidate, entry.Name(), plugin, boundary)
	}
}

func (l *loader) generatedSkill(directory string) bool {
	if _, err := os.Lstat(filepath.Join(directory, originMarker)); err == nil {
		return true
	}
	plugins, err := resolvePotential(filepath.Join(l.agents, "plugins"))
	if err == nil && within(plugins, directory) {
		return true
	}
	for _, root := range l.pluginRoots {
		if within(root, directory) {
			return true
		}
	}
	return false
}

func (l *loader) loadSkill(directory, dirname, plugin, boundary string) {
	location := filepath.Join(directory, "SKILL.md")
	if _, err := os.Lstat(location); os.IsNotExist(err) {
		return
	}
	path, err := linkedReadPath(boundary, location, false)
	if err != nil {
		l.warn(location, err)
		return
	}
	data, err := readBounded(path)
	if err != nil {
		l.warn(location, err)
		return
	}
	name, err := validateSkill(data, dirname)
	if err != nil {
		l.warn(location, err)
		return
	}
	harnesses, err := skillHarnesses(data)
	if err != nil {
		l.warn(location, err)
		return
	}
	if err = validateSkillTree(directory, boundary); err != nil {
		l.warn(location, err)
		return
	}
	if plugin != "" {
		name = plugin + "." + name
	}
	for _, previous := range l.config.Skills {
		if previous.Name == name {
			l.warn(location, fmt.Errorf("skill %q collides with %s; keeping the first entry", name, previous.Path))
			return
		}
	}
	l.config.Skills = append(l.config.Skills, Skill{Name: name, Path: directory, File: path, Harnesses: harnesses})
}

// skillHarnesses reads the optional Agent Skills string metadata.harness.
// A missing key means all configured harnesses.
func skillHarnesses(data []byte) ([]string, error) {
	frontmatter, err := skillFrontmatter(data)
	if err != nil {
		return nil, err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(frontmatter, &document); err != nil {
		return nil, err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("skill frontmatter must be a mapping")
	}
	count := 0
	if err := validateYAMLNode(&document, 0, &count); err != nil {
		return nil, err
	}
	fields := yamlFields(document.Content[0])
	metadata := fields["metadata"]
	if metadata == nil {
		return nil, nil
	}
	if metadata.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("skill metadata must be a string map")
	}
	value := yamlFields(metadata)["harness"]
	if value == nil {
		return nil, nil
	}
	if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
		return nil, fmt.Errorf("metadata.harness must be a string")
	}
	if strings.TrimSpace(value.Value) == "" {
		return nil, fmt.Errorf("metadata.harness cannot be empty")
	}
	if strings.TrimSpace(value.Value) == "none" {
		// An explicit empty set is distinct from an absent metadata.harness,
		// which means all selected harnesses. This allows the TUI to disable
		// a skill globally without removing its canonical source.
		return []string{"none"}, nil
	}
	seen := map[string]bool{}
	var result []string
	for _, item := range strings.Split(value.Value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("metadata.harness contains an empty harness")
		}
		if item == "none" {
			return nil, fmt.Errorf("metadata.harness none must be used alone")
		}
		switch item {
		case "claude-code", "codex", "cursor", "opencode", "gemini", "antigravity", "vscode":
		default:
			return nil, fmt.Errorf("unknown metadata.harness %q", item)
		}
		if !seen[item] {
			result = append(result, item)
			seen[item] = true
		}
	}
	return result, nil
}

func validateSkill(data []byte, dirname string) (string, error) {
	frontmatter, err := skillFrontmatter(data)
	if err != nil {
		return "", err
	}
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(frontmatter))
	if err = decoder.Decode(&document); err != nil {
		return "", fmt.Errorf("invalid skill frontmatter: %w", err)
	}
	var extra yaml.Node
	if err = decoder.Decode(&extra); err != io.EOF {
		return "", fmt.Errorf("skill frontmatter must contain one YAML document")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("skill frontmatter must be a mapping")
	}
	count := 0
	if err = validateYAMLNode(&document, 0, &count); err != nil {
		return "", err
	}
	fields := yamlFields(document.Content[0])
	name, err := yamlString(fields, "name", true)
	if err != nil {
		return "", err
	}
	if !validSkillName(name) || name != dirname {
		return "", fmt.Errorf("skill name must match directory %q and use 1-64 lowercase letters, digits or single hyphens with alphanumeric ends", dirname)
	}
	description, err := yamlString(fields, "description", true)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(description) == "" || utf8.RuneCountInString(description) > 1024 {
		return "", fmt.Errorf("skill description must contain 1-1024 characters")
	}
	for _, key := range []string{"license", "allowed-tools", "compatibility"} {
		value, err := yamlString(fields, key, false)
		if err != nil {
			return "", err
		}
		if key == "compatibility" && fields[key] != nil && (strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > 500) {
			return "", fmt.Errorf("skill compatibility must contain 1-500 characters")
		}
	}
	if metadata := fields["metadata"]; metadata != nil {
		if metadata.Kind != yaml.MappingNode {
			return "", fmt.Errorf("skill metadata must be a string map")
		}
		for i := 0; i < len(metadata.Content); i += 2 {
			if metadata.Content[i].Tag != "!!str" || metadata.Content[i+1].Kind != yaml.ScalarNode || metadata.Content[i+1].Tag != "!!str" {
				return "", fmt.Errorf("skill metadata keys and values must be strings")
			}
		}
	}
	return name, nil
}

func skillFrontmatter(data []byte) ([]byte, error) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) < 2 || string(bytes.TrimSuffix(lines[0], []byte("\r"))) != "---" {
		return nil, fmt.Errorf("SKILL.md must begin with YAML frontmatter delimited by ---")
	}
	size := 0
	for i := 1; i < len(lines); i++ {
		if string(bytes.TrimSuffix(lines[i], []byte("\r"))) == "---" {
			return bytes.Join(lines[1:i], []byte("\n")), nil
		}
		size += len(lines[i]) + 1
		if size > maxFrontmatterBytes {
			return nil, fmt.Errorf("skill frontmatter exceeds %d bytes", maxFrontmatterBytes)
		}
	}
	return nil, fmt.Errorf("skill frontmatter has no closing --- delimiter")
}

func validSkillName(name string) bool {
	length := utf8.RuneCountInString(name)
	if length < 1 || length > 64 || strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") || strings.Contains(name, "--") {
		return false
	}
	for _, char := range name {
		if char != '-' && !unicode.IsLower(char) && !unicode.IsDigit(char) {
			return false
		}
	}
	return true
}

func yamlFields(node *yaml.Node) map[string]*yaml.Node {
	result := map[string]*yaml.Node{}
	for i := 0; i < len(node.Content); i += 2 {
		result[node.Content[i].Value] = node.Content[i+1]
	}
	return result
}

func yamlString(fields map[string]*yaml.Node, key string, required bool) (string, error) {
	node := fields[key]
	if node == nil {
		if required {
			return "", fmt.Errorf("skill frontmatter requires %s", key)
		}
		return "", nil
	}
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return "", fmt.Errorf("skill %s must be a string", key)
	}
	return node.Value, nil
}

func validateYAMLNode(node *yaml.Node, depth int, count *int) error {
	*count++
	if depth > 32 || *count > 4096 {
		return fmt.Errorf("skill YAML exceeds depth or node limit")
	}
	if node.Kind == yaml.AliasNode || node.Anchor != "" {
		return fmt.Errorf("skill YAML anchors and aliases are unsupported")
	}
	if node.Tag != "" && !strings.HasPrefix(node.Tag, "!!") {
		return fmt.Errorf("custom YAML tags are unsupported")
	}
	if node.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return fmt.Errorf("skill YAML mapping keys must be strings")
			}
			if seen[key.Value] {
				return fmt.Errorf("duplicate YAML key %q", key.Value)
			}
			seen[key.Value] = true
		}
	}
	for _, child := range node.Content {
		if err := validateYAMLNode(child, depth+1, count); err != nil {
			return err
		}
	}
	return nil
}

func validateSkillTree(root, boundary string) error {
	if _, err := linkedReadPath(boundary, root, false); err != nil {
		return err
	}
	_, err := fsstate.Capture(root, fsstate.Options{FollowLinks: true, MetadataOnly: true})
	return err
}
