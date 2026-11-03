package tui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"agentmux/internal/config"
	"agentmux/internal/pathutil"

	"github.com/gofrs/flock"
	"gopkg.in/yaml.v3"
)

const (
	maxSkillFileBytes = 1 << 20
	maxSkillYAMLBytes = 64 << 10
)

// The order is stable when the TUI writes the comma-separated harness filter.
// Include the legacy vscode identifier to preserve its existing availability
// when changing a filter that previously granted all supported harnesses.
var knownSkillHarnesses = []string{"claude-code", "codex", "cursor", "opencode", "gemini", "antigravity", "vscode"}

// setSkillHarness updates the source skill's portable metadata.harness field.
// A selected skill is the snapshot obtained from config.Load when the modal
// was opened; stale filters, stale paths, and concurrent file changes abort.
// It never writes to installed native harness directories or project files.
func setSkillHarness(root string, selected config.Skill, harness string, enabled bool) error {
	if !slices.Contains(knownSkillHarnesses, harness) || harness == "vscode" {
		return fmt.Errorf("%q is unavailable for user-scope skill management", harness)
	}
	current, err := config.Load(root)
	if err != nil {
		return fmt.Errorf("load current skill configuration: %w", err)
	}
	if !slices.Contains(current.Settings.Harnesses, harness) {
		return fmt.Errorf("harness %q is disabled; refresh before editing skills", harness)
	}
	return applySkillHarnessSelection(root, selected, map[string]bool{harness: enabled})
}

// setSkillHarnessSelection applies one full NewMultiSelect result with a
// single backup and a single checked update to the source SKILL.md. Unlike a
// per-harness toggle, the full form can represent metadata.harness: none.
func setSkillHarnessSelection(root string, selected config.Skill, chosen []string) error {
	desired := make(map[string]bool, len(knownSkillHarnesses))
	for _, harness := range chosen {
		if !slices.Contains(knownSkillHarnesses, harness) || desired[harness] {
			return fmt.Errorf("invalid or duplicate skill harness %q", harness)
		}
		desired[harness] = true
	}
	for _, harness := range knownSkillHarnesses {
		if !desired[harness] {
			desired[harness] = false
		}
	}
	return applySkillHarnessSelection(root, selected, desired)
}

func applySkillHarnessSelection(root string, selected config.Skill, desired map[string]bool) error {
	for harness := range desired {
		if !slices.Contains(knownSkillHarnesses, harness) {
			return fmt.Errorf("unknown skill harness %q", harness)
		}
	}
	current, err := config.Load(root)
	if err != nil {
		return fmt.Errorf("load current skill configuration: %w", err)
	}
	agents := filepath.Join(current.Root, ".agentmux")
	if err := requireRealDirectory(agents); err != nil {
		return fmt.Errorf("source directory: %w", err)
	}
	// The synchronizer also reads SKILL.md sources while applying a plan.
	// Coordinate with its lock to avoid a stale or mixed native projection.
	lockPath := filepath.Join(agents, "sync.lock")
	if err := rejectLinkedFile(lockPath); err != nil {
		return err
	}
	lock := flock.New(lockPath)
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 35*time.Millisecond)
	if err != nil {
		return fmt.Errorf("skill edit lock: %w", err)
	}
	if !locked {
		return errors.New("skill settings are being edited elsewhere; retry after refreshing")
	}
	defer lock.Unlock()

	current, err = config.Load(current.Root)
	if err != nil {
		return fmt.Errorf("reload current skill configuration: %w", err)
	}
	var found *config.Skill
	for i := range current.Skills {
		if current.Skills[i].Name == selected.Name {
			found = &current.Skills[i]
			break
		}
	}
	if found == nil || filepath.Clean(found.Path) != filepath.Clean(selected.Path) || !slices.Equal(found.Harnesses, selected.Harnesses) {
		return fmt.Errorf("skill %q changed since it was loaded; refresh before editing", selected.Name)
	}
	unchanged := true
	for harness, enabled := range desired {
		if config.Allows(found.Harnesses, harness) != enabled {
			unchanged = false
		}
	}
	if unchanged {
		return errNoChange
	}
	path, err := checkedSkillSource(current.Root, *found)
	if err != nil {
		return err
	}
	original, err := readBoundedSkill(path)
	if err != nil {
		return err
	}
	updated, err := editSkillFrontmatterAvailability(original, found.Harnesses, desired)
	if err != nil {
		return err
	}
	for harness, enabled := range desired {
		if err := validateEditedSkill(updated, harness, enabled); err != nil {
			return err
		}
	}
	if bytes.Equal(original, updated) {
		return errNoChange
	}
	if err := skillFileUnchanged(path, original); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}

	temporary, err := os.CreateTemp(filepath.Dir(path), ".agentmux-skill-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary SKILL.md: %w", err)
	}
	defer os.Remove(temporary.Name())
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(updated); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	backupDir := filepath.Join(agents, "backups", "skills")
	if err := createRealSubdirectories(agents, "backups", "skills"); err != nil {
		return fmt.Errorf("prepare skill backups: %w", err)
	}
	// Backups live outside the skill tree, so sync/install never copies them.
	identity := sha256.Sum256([]byte(filepath.Clean(path)))
	backup := filepath.Join(backupDir, fmt.Sprintf("%s-%x-%s.bak", found.Name, identity[:4], time.Now().UTC().Format("20060102T150405.000000000Z")))
	if err := writeBackup(backup, original); err != nil {
		return fmt.Errorf("back up original SKILL.md: %w", err)
	}
	if err := skillFileUnchanged(path, original); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return fmt.Errorf("atomically replace SKILL.md: %w", err)
	}
	return nil
}

func checkedSkillSource(root string, skill config.Skill) (string, error) {
	relative, err := filepath.Rel(root, skill.Path)
	if err != nil {
		return "", err
	}
	components := strings.Split(filepath.ToSlash(relative), "/")
	standalone := len(components) == 3 && components[0] == ".agentmux" && components[1] == "skills"
	plugin := len(components) == 5 && components[0] == ".agentmux" && components[1] == "plugins" && components[3] == "skills"
	if !standalone && !plugin {
		return "", fmt.Errorf("skill %q is outside canonical user skill/plugin sources; project edits are forbidden", skill.Name)
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return "", fmt.Errorf("invalid skill source path %q", skill.Path)
		}
	}
	current := root
	for _, component := range components {
		current = filepath.Join(current, component)
		if err := requireRealDirectory(current); err != nil {
			return "", fmt.Errorf("linked or invalid skill directory %s: %w", current, err)
		}
	}
	path := filepath.Join(current, "SKILL.md")
	if _, err := readBoundedSkill(path); err != nil {
		return "", err
	}
	return path, nil
}

func requireRealDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || pathutil.IsRedirected(info) {
		return errors.New("expected a real, non-linked directory")
	}
	return nil
}

func rejectLinkedFile(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s: expected a regular file", path)
	}
	return nil
}

func createRealSubdirectories(base string, parts ...string) error {
	current := base
	if err := requireRealDirectory(current); err != nil {
		return err
	}
	for _, part := range parts {
		current = filepath.Join(current, part)
		if err := os.Mkdir(current, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
		if err := requireRealDirectory(current); err != nil {
			return err
		}
	}
	return nil
}

func readBoundedSkill(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSkillFileBytes {
		return nil, fmt.Errorf("%s: skill source must be a regular file smaller than 1 MiB", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxSkillFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSkillFileBytes || !utf8.Valid(data) {
		return nil, fmt.Errorf("%s: skill source must be UTF-8 and smaller than 1 MiB", path)
	}
	return data, nil
}

func skillFileUnchanged(path string, original []byte) error {
	latest, err := readBoundedSkill(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(latest, original) {
		return errors.New("SKILL.md changed during editing; refresh before retrying")
	}
	return nil
}

// editSkillFrontmatter edits only the metadata.harness scalar (or adds that
// mapping key). Splitting on newlines and replacing one value leaves the body,
// comments, key order and existing newline style untouched. Ambiguous YAML
// shapes are refused rather than serialized into a different document.
func editSkillFrontmatter(data []byte, originalFilter []string, harness string, enabled bool) ([]byte, error) {
	return editSkillFrontmatterAvailability(data, originalFilter, map[string]bool{harness: enabled})
}

func editSkillFrontmatterAvailability(data []byte, originalFilter []string, changes map[string]bool) ([]byte, error) {
	allowed := make(map[string]bool)
	if len(originalFilter) == 0 {
		for _, id := range knownSkillHarnesses {
			allowed[id] = true
		}
	} else {
		for _, id := range originalFilter {
			allowed[id] = true
		}
	}
	for harness, enabled := range changes {
		allowed[harness] = enabled
	}
	var filtered []string
	for _, id := range knownSkillHarnesses {
		if allowed[id] {
			filtered = append(filtered, id)
		}
	}
	csv := strings.Join(filtered, ", ")
	if len(filtered) == 0 {
		// The canonical parser reserves "none" for an explicit empty set.
		// An absent/empty metadata.harness means the opposite: all harnesses.
		csv = "none"
	}
	lines, closing, root, err := parseSkillYAML(data)
	if err != nil {
		return nil, err
	}
	metadataKey, metadata := findYAMLKey(root, "metadata")
	cr := ""
	if strings.HasSuffix(lines[0], "\r") {
		cr = "\r"
	}
	switch {
	case metadataKey == nil:
		insertion := []string{"metadata:" + cr, "  harness: " + csv + cr}
		lines = slices.Insert(lines, closing, insertion...)
	case metadata.Kind != yaml.MappingNode || metadata.Style&yaml.FlowStyle != 0 || metadata.Style&yaml.TaggedStyle != 0:
		return nil, errors.New("metadata is a complex YAML map; use block-style metadata before editing skill availability")
	default:
		if err := plainYAMLKeyLine(lines, metadataKey, "metadata"); err != nil {
			return nil, err
		}
		harnessKey, value := findYAMLKey(metadata, "harness")
		if harnessKey == nil {
			indent := metadataKey.Column + 1 // 2 spaces deeper than a block key.
			if len(metadata.Content) > 0 {
				indent = metadata.Content[0].Column - 1
			}
			if indent <= metadataKey.Column-1 {
				return nil, errors.New("cannot infer a safe indentation for metadata.harness")
			}
			insertion := strings.Repeat(" ", indent) + "harness: " + csv + cr
			lines = slices.Insert(lines, metadataKey.Line+1, insertion)
		} else {
			if err := plainYAMLKeyLine(lines, harnessKey, "harness"); err != nil {
				return nil, err
			}
			if value.Kind != yaml.ScalarNode || value.Tag != "!!str" || value.Style&(yaml.FoldedStyle|yaml.LiteralStyle|yaml.TaggedStyle) != 0 {
				return nil, errors.New("metadata.harness uses a complex YAML value; use one plain or quoted line")
			}
			index := harnessKey.Line
			updated, err := replaceHarnessValue(lines[index], harnessKey.Column-1, value, csv)
			if err != nil {
				return nil, err
			}
			lines[index] = updated
		}
	}
	result := []byte(strings.Join(lines, "\n"))
	if bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
		result = append([]byte{0xef, 0xbb, 0xbf}, result...)
	}
	return result, nil
}

// parseSkillYAML produces line positions corresponding to yaml.Node.Line.
// The first YAML line is at lines[1] because lines[0] is the --- delimiter.
func parseSkillYAML(data []byte) ([]string, int, *yaml.Node, error) {
	lines := strings.Split(string(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})), "\n")
	if len(lines) < 3 || strings.TrimSuffix(lines[0], "\r") != "---" {
		return nil, 0, nil, errors.New("SKILL.md requires YAML frontmatter starting with ---")
	}
	closing := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSuffix(lines[i], "\r") == "---" {
			closing = i
			break
		}
	}
	if closing < 0 {
		return nil, 0, nil, errors.New("SKILL.md is missing a closing --- delimiter")
	}
	frontmatter := strings.Join(lines[1:closing], "\n")
	if len(frontmatter) > maxSkillYAMLBytes {
		return nil, 0, nil, errors.New("YAML frontmatter exceeds 64 KiB")
	}
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(frontmatter), &document); err != nil {
		return nil, 0, nil, fmt.Errorf("parse SKILL.md YAML: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, 0, nil, errors.New("SKILL.md frontmatter must contain a YAML mapping")
	}
	if err := uniqueYAMLKeys(&document); err != nil {
		return nil, 0, nil, err
	}
	return lines, closing, document.Content[0], nil
}

func uniqueYAMLKeys(node *yaml.Node) error {
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]bool)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return errors.New("SKILL.md frontmatter has a complex YAML key")
			}
			if seen[key.Value] {
				return fmt.Errorf("SKILL.md contains duplicate YAML key %q", key.Value)
			}
			seen[key.Value] = true
		}
	}
	for _, child := range node.Content {
		if err := uniqueYAMLKeys(child); err != nil {
			return err
		}
	}
	return nil
}

func findYAMLKey(parent *yaml.Node, name string) (*yaml.Node, *yaml.Node) {
	if parent == nil || parent.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == name {
			return parent.Content[i], parent.Content[i+1]
		}
	}
	return nil, nil
}

// plainYAMLKeyLine ensures edits target simple indented mapping entries.
// Quoted keys, flow maps, tags and non-standard indentation require manual
// editing to avoid changing their meaning or stripping comments.
func plainYAMLKeyLine(lines []string, key *yaml.Node, expected string) error {
	index := key.Line
	if index <= 0 || index >= len(lines) || key.Column <= 0 {
		return errors.New("YAML key location is outside the editable frontmatter")
	}
	line := strings.TrimSuffix(lines[index], "\r")
	start := key.Column - 1
	if start > len(line) || strings.TrimSpace(line[:start]) != "" || !strings.HasPrefix(line[start:], expected+":") {
		return fmt.Errorf("YAML %s key has complex formatting; edit it manually", expected)
	}
	if expected == "metadata" {
		trailing := strings.TrimSpace(line[start+len(expected)+1:])
		if trailing != "" && !strings.HasPrefix(trailing, "#") {
			return errors.New("metadata must use a block-style YAML mapping for safe edits")
		}
	}
	return nil
}

func replaceHarnessValue(fullLine string, keyStart int, value *yaml.Node, csv string) (string, error) {
	cr := ""
	line := fullLine
	if strings.HasSuffix(line, "\r") {
		line, cr = strings.TrimSuffix(line, "\r"), "\r"
	}
	colon := keyStart + len("harness:")
	if colon > len(line) {
		return "", errors.New("metadata.harness key extends past its YAML line")
	}
	begin := colon
	for begin < len(line) && (line[begin] == ' ' || line[begin] == '\t') {
		begin++
	}
	if begin == len(line) {
		return "", errors.New("metadata.harness has no editable value")
	}
	end, valueText, err := editableValueEnd(line, begin, value, csv)
	if err != nil {
		return "", err
	}
	return line[:begin] + valueText + line[end:] + cr, nil
}

func editableValueEnd(line string, begin int, value *yaml.Node, csv string) (int, string, error) {
	quoted := byte(0)
	if value.Style&yaml.SingleQuotedStyle != 0 {
		quoted = '\''
	} else if value.Style&yaml.DoubleQuotedStyle != 0 {
		quoted = '"'
	}
	if quoted != 0 {
		if line[begin] != quoted {
			return 0, "", errors.New("cannot safely locate quoted metadata.harness value")
		}
		for i := begin + 1; i < len(line); i++ {
			if quoted == '"' && line[i] == '\\' {
				i++
				continue
			}
			if line[i] == quoted {
				if quoted == '\'' && i+1 < len(line) && line[i+1] == '\'' {
					i++
					continue
				}
				tail := strings.TrimSpace(line[i+1:])
				if tail != "" && !strings.HasPrefix(tail, "#") {
					return 0, "", errors.New("metadata.harness contains multiline or complex quoted YAML")
				}
				return i + 1, string(quoted) + csv + string(quoted), nil
			}
		}
		return 0, "", errors.New("metadata.harness quoted value spans multiple lines")
	}
	if value.Style != 0 {
		return 0, "", errors.New("metadata.harness uses unsupported YAML syntax; use a simple string")
	}
	end := len(line)
	for i := begin; i < len(line); i++ {
		if line[i] == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
			end = i
			break
		}
	}
	for end > begin && (line[end-1] == ' ' || line[end-1] == '\t') {
		end--
	}
	if end == begin {
		return 0, "", errors.New("metadata.harness has an empty value")
	}
	return end, csv, nil
}

func validateEditedSkill(data []byte, harness string, enabled bool) error {
	_, _, root, err := parseSkillYAML(data)
	if err != nil {
		return fmt.Errorf("updated skill validation: %w", err)
	}
	_, metadata := findYAMLKey(root, "metadata")
	_, value := findYAMLKey(metadata, "harness")
	if value == nil || value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
		return errors.New("updated skill must contain string metadata.harness")
	}
	if value.Value == "none" {
		if enabled {
			return errors.New("metadata.harness none cannot enable a harness")
		}
		return nil
	}
	seen := make(map[string]bool)
	for _, field := range strings.Split(value.Value, ",") {
		id := strings.TrimSpace(field)
		if id == "" || !slices.Contains(knownSkillHarnesses, id) || seen[id] {
			return fmt.Errorf("updated metadata.harness contains invalid or duplicate identifier %q", id)
		}
		seen[id] = true
	}
	if seen[harness] != enabled {
		return errors.New("updated metadata.harness did not produce the requested availability")
	}
	return nil
}
