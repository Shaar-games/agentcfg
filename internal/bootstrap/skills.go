package bootstrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"agentcfg/internal/pathutil"
	"gopkg.in/yaml.v3"
)

func discoverSkills(root *os.Root, plan *Plan, harness, source string) {
	path, err := anchoredPath(root, source)
	if err != nil {
		plan.warnSkill(harness, source, "skills path changed or escapes the discovery home")
		return
	}
	entries, err := readDir(root, path)
	if err != nil {
		plan.warnSkill(harness, source, "skills directory is unreadable or exceeds the entry limit")
		return
	}
	for _, entry := range entries {
		if plan.budget.skills <= 0 || plan.budget.entries <= 0 || plan.budget.bytes <= 0 {
			plan.warnSkill(harness, source, "discovery skill budget reached; remaining entries require a separate review")
			break
		}
		plan.budget.skills--
		// The configured skills root may itself be an alias. Its native path is
		// provenance; the anchored relative path is only for reading the target.
		native := filepath.Join(source, entry.Name())
		relative, err := anchoredPath(root, native)
		if err != nil {
			plan.warnSkill(harness, native, "skill source is unreadable or escapes the discovery home")
			continue
		}
		resolved := filepath.Join(root.Name(), relative)
		if within(filepath.Join(root.Name(), ".agentcfg"), resolved) {
			continue
		}
		info, err := root.Stat(relative)
		if err != nil || !info.IsDir() {
			continue
		}
		if _, err := root.Lstat(filepath.Join(relative, ".agentcfg-origin.json")); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			plan.warnSkill(harness, native, "skill origin marker cannot be inspected")
			continue
		}
		if _, err := root.Lstat(filepath.Join(relative, "SKILL.md")); os.IsNotExist(err) {
			continue
		}
		fingerprint, treeSHA256, disabled, err := inspectSkill(root, relative, entry.Name(), plan.budget)
		if err != nil {
			plan.warnSkill(harness, native, "skill is invalid, nonportable, unreadable, redirected or exceeds discovery limits")
			continue
		}
		warnings := []string{}
		if disabled {
			warnings = append(warnings, "SKILL.md metadata.harness disables this skill; metadata is preserved")
		}
		plan.Skills = append(plan.Skills, SkillCandidate{
			Name: entry.Name(), Source: resolved, Sources: []string{resolved},
			Harnesses: []string{harness}, Selected: !disabled, Warnings: warnings, Fingerprint: fingerprint,
			Origins: []Origin{{Harness: harness, Source: native}}, TreeSHA256: treeSHA256,
		})
	}
}

// VerifySkill reopens only a candidate under the supplied home and compares its
// complete portable tree with Discover's fingerprint. Call immediately before
// copying; the copying operation must also use anchored reads to close races.
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
	relative, err := anchoredPath(root, candidate.Source)
	if err != nil {
		return errors.New("selected skill source changed or escapes home")
	}
	actual, treeSHA256, _, err := inspectSkill(root, relative, candidate.Name, nil)
	if err != nil || actual != candidate.Fingerprint || candidate.TreeSHA256 != "" && treeSHA256 != candidate.TreeSHA256 {
		return errors.New("selected skill changed since discovery")
	}
	return nil
}

func inspectSkill(home *os.Root, relative, name string, budget *scanBudget) (string, string, bool, error) {
	root, err := home.OpenRoot(relative)
	if err != nil {
		return "", "", false, err
	}
	defer root.Close()
	data, err := readFile(root, "SKILL.md", budget.limit(maxConfigBytes), true)
	budget.charge(len(data))
	if err != nil {
		return "", "", false, err
	}
	disabled, err := validateSkill(data, name)
	if err != nil {
		return "", "", false, err
	}
	h := sha256.New()
	treeHash := sha256.New()
	count, total := 0, int64(0)
	if err = fingerprintTree(root, ".", h, treeHash, 0, &count, &total, data, budget); err != nil {
		return "", "", false, err
	}
	return hex.EncodeToString(h.Sum(nil)), hex.EncodeToString(treeHash.Sum(nil)), disabled, nil
}

// Nested links (including Windows junctions) are deliberately not portable
// import candidates. The top-level alias may resolve within home; the validated
// tree itself contains only directories and regular files.
func fingerprintTree(root *os.Root, path string, h, treeHash hash.Hash, depth int, count *int, total *int64, skillData []byte, budget *scanBudget) error {
	*count++
	if depth > 64 || *count > maxTreeEntries {
		return errors.New("skill tree limit")
	}
	if budget != nil {
		if budget.entries <= 0 {
			return errors.New("discovery entry budget reached")
		}
		budget.entries--
	}
	info, err := root.Lstat(path)
	if err != nil {
		return err
	}
	if pathutil.IsRedirected(info) {
		return errors.New("skill contains a link")
	}
	if info.IsDir() {
		treeRecord(treeHash, 'd', path)
		fmt.Fprintf(h, "d%d:%s;", len(filepath.ToSlash(path)), filepath.ToSlash(path))
		entries, err := readDir(root, path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := fingerprintTree(root, filepath.Join(path, entry.Name()), h, treeHash, depth+1, count, total, skillData, budget); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return errors.New("skill contains a special file")
	}
	data, err := readFile(root, path, budget.limit(maxAssetBytes), false)
	budget.charge(len(data))
	if err != nil {
		return err
	}
	if path == "SKILL.md" && !bytes.Equal(data, skillData) {
		return errors.New("skill frontmatter changed during discovery")
	}
	after, err := root.Lstat(path)
	if err != nil || pathutil.IsRedirected(after) || !os.SameFile(info, after) ||
		info.Mode() != after.Mode() || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return errors.New("skill file changed during discovery")
	}
	*total += int64(len(data))
	if *total > maxTreeBytes {
		return errors.New("skill tree size limit")
	}
	fmt.Fprintf(h, "f%d:%s:%d:%d:", len(filepath.ToSlash(path)), filepath.ToSlash(path), info.Mode().Perm()&0111, len(data))
	h.Write(data)
	treeRecord(treeHash, 'f', path)
	content := sha256.Sum256(data)
	treeHash.Write(content[:])
	return nil
}

// Match syncer.SkillTreeSHA256's adoption format at the same bounded, anchored
// read as our stricter permission-sensitive fingerprint. The interop test keeps
// this record format in sync without reopening a potentially swapped path.
func treeRecord(h hash.Hash, kind byte, path string) {
	name := []byte(filepath.ToSlash(path))
	h.Write([]byte{kind})
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(name)))
	h.Write(size[:])
	h.Write(name)
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
