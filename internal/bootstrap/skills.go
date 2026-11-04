package bootstrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"agentcfg/internal/fsstate"
	"agentcfg/internal/pathutil"
	"gopkg.in/yaml.v3"
)

func discoverSkills(root *os.Root, plan *Plan, harness, source string) {
	entries, err := fsstate.ReadDir(source, maxTreeEntries)
	if err != nil {
		plan.warnSkill(harness, source, "skills directory is unreadable or exceeds the entry limit")
		return
	}
	for _, entry := range entries {
		// Private work from older versions must never become an import candidate.
		if strings.HasPrefix(entry.Name(), ".agentcfg-layout-") || strings.HasPrefix(entry.Name(), ".agentcfg-stage-") {
			continue
		}
		if plan.budget.skills <= 0 || plan.budget.entries <= 0 || plan.budget.bytes <= 0 {
			plan.warnSkill(harness, source, "discovery skill budget reached; remaining entries require a separate review")
			break
		}
		plan.budget.skills--
		// The configured skills root may itself be an alias. Its native path is
		// provenance; the resolved path is used only for reading the target.
		native := filepath.Join(source, entry.Name())
		resolved, err := pathutil.Resolve(native)
		if err != nil {
			plan.warnSkill(harness, native, "skill source is unreadable or has an invalid link")
			continue
		}
		physical := resolved
		if physicalHome, err := pathutil.Resolve(root.Name()); err == nil {
			if relative, err := relativeWithin(physicalHome, resolved); err == nil {
				resolved = filepath.Join(root.Name(), relative)
			}
		}
		if within(filepath.Join(root.Name(), ".agentcfg"), resolved) {
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			continue
		}
		if _, err := os.Lstat(filepath.Join(resolved, ".agentcfg-origin.json")); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			plan.warnSkill(harness, native, "skill origin marker cannot be inspected")
			continue
		}
		if _, err := os.Lstat(filepath.Join(resolved, "SKILL.md")); os.IsNotExist(err) {
			continue
		}
		fingerprint, treeSHA256, layout, disabled, err := inspectSkill(root, native, entry.Name(), plan.budget)
		if err != nil {
			plan.warnSkill(harness, native, "skill is invalid, nonportable, unreadable or exceeds discovery limits")
			continue
		}
		warnings := []string{}
		if disabled {
			warnings = append(warnings, "SKILL.md metadata.harness disables this skill; metadata is preserved")
		}
		plan.Skills = append(plan.Skills, SkillCandidate{
			Name: entry.Name(), Source: resolved, Sources: []string{resolved},
			Harnesses: []string{harness}, Selected: !disabled, Warnings: warnings, Fingerprint: fingerprint,
			Origins: []Origin{{Harness: harness, Source: native, Target: physical, Layout: layout}}, TreeSHA256: treeSHA256,
		})
	}
}

// VerifySkill reopens a discovered user skill and compares its complete portable
// tree with Discover's fingerprint. The copy operation also verifies the source.
func VerifySkill(home string, candidate SkillCandidate) error {
	if !filepath.IsAbs(home) || candidate.Fingerprint == "" || !filepath.IsAbs(candidate.Source) {
		return errors.New("invalid skill verification input")
	}
	resolved, err := pathutil.Resolve(home)
	if err != nil {
		return errors.New("cannot verify skill home")
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return errors.New("cannot verify skill home")
	}
	defer root.Close()
	return verifySkill(root, candidate)
}

func verifySkill(root *os.Root, candidate SkillCandidate) error {
	actual, treeSHA256, _, _, err := inspectSkill(root, candidate.Source, candidate.Name, nil)
	if err != nil || actual != candidate.Fingerprint || candidate.TreeSHA256 != "" && treeSHA256 != candidate.TreeSHA256 {
		return errors.New("selected skill changed since discovery")
	}
	return nil
}

func inspectSkill(home *os.Root, source, name string, budget *scanBudget) (string, string, string, bool, error) {
	if !filepath.IsAbs(source) {
		source = filepath.Join(home.Name(), source)
	}
	data, err := fsstate.ReadFile(filepath.Join(source, "SKILL.md"), "", budget.limit(maxConfigBytes))
	budget.charge(len(data))
	if err != nil {
		return "", "", "", false, err
	}
	disabled, err := validateSkill(data, name)
	if err != nil {
		return "", "", "", false, err
	}
	options := fsstate.Options{FollowLinks: true, MaxEntries: maxTreeEntries, MaxFile: maxAssetBytes, MaxBytes: budget.limit(maxTreeBytes)}
	if budget != nil && budget.entries < options.MaxEntries {
		options.MaxEntries = budget.entries
		if options.MaxEntries == 0 {
			options.MaxEntries = -1
		}
	}
	snapshot, err := fsstate.Capture(source, options)
	if snapshot != nil && budget != nil {
		budget.entries -= snapshot.Entries
		budget.bytes -= snapshot.Bytes
	}
	if err != nil {
		return "", "", "", false, err
	}
	hash := sha256.Sum256(data)
	if snapshot.Files["SKILL.md"] != hex.EncodeToString(hash[:]) {
		return "", "", "", false, errors.New("skill frontmatter changed during discovery")
	}
	return snapshot.Fingerprint, snapshot.Content, snapshot.Layout, disabled, nil
}

func validateSkill(data []byte, dirname string) (bool, error) {
	fail := func() (bool, error) { return false, errors.New("invalid portable skill frontmatter") }
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) < 2 || string(bytes.TrimSuffix(lines[0], []byte("\r"))) != "---" {
		return fail()
	}
	end, size := 0, 0
	for i := 1; i < len(lines); i++ {
		if string(bytes.TrimSuffix(lines[i], []byte("\r"))) == "---" {
			end = i
			break
		}
		size += len(lines[i]) + 1
		if size > 64<<10 {
			return fail()
		}
	}
	if end == 0 {
		return fail()
	}
	decoder := yaml.NewDecoder(bytes.NewReader(bytes.Join(lines[1:end], []byte("\n"))))
	var document, extra yaml.Node
	if decoder.Decode(&document) != nil || decoder.Decode(&extra) != io.EOF ||
		len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return fail()
	}
	count := 0
	if !validYAML(&document, 0, &count) {
		return fail()
	}
	fields := yamlFields(document.Content[0])
	name, nameOK := yamlString(fields["name"])
	description, descriptionOK := yamlString(fields["description"])
	if !nameOK || name != dirname || !validSkillName(name) || !descriptionOK ||
		strings.TrimSpace(description) == "" || utf8.RuneCountInString(description) > 1024 {
		return fail()
	}
	for _, key := range []string{"license", "allowed-tools", "compatibility"} {
		if field := fields[key]; field != nil {
			value, ok := yamlString(field)
			if !ok || key == "compatibility" && (strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > 500) {
				return fail()
			}
		}
	}
	if metadata := fields["metadata"]; metadata != nil {
		if metadata.Kind != yaml.MappingNode {
			return fail()
		}
		for i := 0; i < len(metadata.Content); i += 2 {
			if _, ok := yamlString(metadata.Content[i+1]); !ok {
				return fail()
			}
		}
		if field := yamlFields(metadata)["harness"]; field != nil {
			value, _ := yamlString(field)
			value = strings.TrimSpace(value)
			if value == "none" {
				return true, nil
			}
			for _, item := range strings.Split(value, ",") {
				switch strings.TrimSpace(item) {
				case "claude-code", "codex", "cursor", "opencode", "gemini", "antigravity", "vscode":
				default:
					return fail()
				}
			}
		}
	}
	return false, nil
}

func validSkillName(name string) bool {
	if utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 64 ||
		strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") || strings.Contains(name, "--") {
		return false
	}
	for _, c := range name {
		if c != '-' && !unicode.IsLower(c) && !unicode.IsDigit(c) {
			return false
		}
	}
	return true
}

func yamlFields(node *yaml.Node) map[string]*yaml.Node {
	fields := map[string]*yaml.Node{}
	for i := 0; i < len(node.Content); i += 2 {
		fields[node.Content[i].Value] = node.Content[i+1]
	}
	return fields
}

func yamlString(node *yaml.Node) (string, bool) {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return "", false
	}
	return node.Value, true
}

func validYAML(node *yaml.Node, depth int, count *int) bool {
	*count++
	if depth > 32 || *count > 4096 || node.Kind == yaml.AliasNode || node.Anchor != "" ||
		(node.Tag != "" && !strings.HasPrefix(node.Tag, "!!")) {
		return false
	}
	if node.Kind == yaml.MappingNode {
		if len(node.Content)%2 != 0 {
			return false
		}
		seen := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key, ok := yamlString(node.Content[i])
			if !ok || seen[key] {
				return false
			}
			seen[key] = true
		}
	}
	for _, child := range node.Content {
		if !validYAML(child, depth+1, count) {
			return false
		}
	}
	return true
}
