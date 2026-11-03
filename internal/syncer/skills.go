package syncer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentcfg/internal/config"
	"agentcfg/internal/pathutil"
)

type skillRecord struct {
	Source      string `json:"source"`
	SourceHash  string `json:"sourceHash"`
	Fingerprint string `json:"fingerprint"`
	Mode        string `json:"mode"`
}

type skillChange struct {
	source, sourceHash, beforeHash, mode string
	remove                               bool
	adoptionHash                         string
}

func treeFingerprint(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		return "link:" + filepath.Clean(target), nil
	}
	if redirected(info) {
		return "", fmt.Errorf("refusing a junction or reparse point as a managed skill tree: %s", path)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("skill path is not a directory: %s", path)
	}
	hash := sha256.New()
	err = filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(path, current)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if redirected(info) {
			return fmt.Errorf("skill tree contains a symlink; use a direct source directory without nested links: %s", current)
		}
		if !entry.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported special file in skill: %s", current)
		}
		_, _ = fmt.Fprintf(hash, "%s\x00%d\x00", filepath.ToSlash(rel), info.Mode())
		if entry.IsDir() {
			return nil
		}
		file, err := os.Open(current)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		return "", err
	}
	return "tree:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func (p *Plan) planSkills(t target, c *config.Config, selected bool, mode string, old manifest) error {
	desired := map[string]config.Skill{}
	for _, skill := range c.Skills {
		destination := filepath.Join(t.skills, filepath.Base(skill.Path))
		if filepath.Clean(destination) == filepath.Clean(skill.Path) {
			if selected && !config.Allows(skill.Harnesses, t.name) {
				p.Warnings = append(p.Warnings, fmt.Sprintf("%s discovers canonical skill %s natively; harness filters cannot hide source files from a native scanner", t.name, skill.Name))
			}
			continue // Canonical source is already a native location. Never adopt it.
		}
		if !selected || !config.Allows(skill.Harnesses, t.name) {
			continue
		}
		if other, exists := desired[destination]; exists {
			return fmt.Errorf("skills %s and %s both install as %s; rename one at its source", other.Name, skill.Name, destination)
		}
		desired[destination] = skill
		if t.name == "codex" && len(skill.Harnesses) != 0 {
			p.Warnings = append(p.Warnings, "Codex skill destination .agents/skills may also be discovered by other harnesses; filters control generated files, not native discovery")
		}
	}
	paths := map[string]bool{}
	for path := range desired {
		paths[path] = true
	}
	for path := range old.Skills {
		if filepath.Dir(path) == t.skills {
			paths[path] = true
		}
	}
	if len(paths) == 0 {
		return nil // No generated skill is needed or previously owned here.
	}
	if err := ensureNoSymlinkParents(p.base, t.skills); err != nil {
		return err
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	for _, destination := range ordered {
		if err := p.planSkill(destination, desired[destination], desired[destination].Path != "", mode, old.Skills[destination]); err != nil {
			return err
		}
	}
	return nil
}

func (p *Plan) planSkill(destination string, skill config.Skill, wanted bool, mode string, previous skillRecord) error {
	var adoption SkillAdoption
	if p.adoption != nil {
		adoption = p.adoption.skills[destination]
	}
	fingerprint := treeFingerprint
	if adoption.CanonicalName != "" {
		fingerprint = adoptionTreeFingerprint
	}
	current, err := fingerprint(destination)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if previous.Fingerprint != "" && (!exists || current != previous.Fingerprint) {
		return fmt.Errorf("managed skill was modified or removed manually: %s", destination)
	}
	if !wanted {
		p.Changes = append(p.Changes, Change{Path: destination, Action: "remove managed skill", exists: exists,
			skill: &skillChange{beforeHash: current, remove: true}})
		return nil
	}
	if exists && previous.Fingerprint == "" && adoption.CanonicalName == "" {
		return fmt.Errorf("skill destination exists and is not owned by agentcfg: %s", destination)
	}
	source, err := pathutil.Resolve(skill.Path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(source, destination)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refusing to copy a skill into itself: %s", destination)
	}
	sourceHash, err := fingerprint(source)
	if err != nil {
		return err
	}
	if adoption.CanonicalName != "" {
		if !exists || adoption.CanonicalName != skill.Name {
			return fmt.Errorf("scanner-approved skill destination no longer matches %q: %s", skill.Name, destination)
		}
		nativeHash, nativeErr := SkillTreeSHA256(destination)
		canonicalHash, sourceErr := SkillTreeSHA256(source)
		if nativeErr != nil {
			return nativeErr
		}
		if sourceErr != nil {
			return sourceErr
		}
		if !strings.EqualFold(nativeHash, adoption.TreeSHA256) || nativeHash != canonicalHash {
			return fmt.Errorf("skill adoption requires the scanned native tree and canonical source to be byte-identical: %s", destination)
		}
		adoption.TreeSHA256 = nativeHash
		p.Warnings = append(p.Warnings, fmt.Sprintf("explicitly adopt existing native skill %q for %s (%s)", skill.Name, adoption.Harness, destination))
	}
	record := skillRecord{Source: source, SourceHash: sourceHash, Mode: mode}
	p.state.Skills[destination] = record
	compatibleMode := mode == "auto" || mode == previous.Mode
	if adoption.CanonicalName == "" && exists && compatibleMode && previous.Source == source && (previous.Mode == "symlink" || previous.SourceHash == sourceHash) {
		previous.SourceHash = sourceHash
		p.state.Skills[destination] = previous
		return nil
	}
	action := "sync skill (" + mode + ")"
	if adoption.CanonicalName != "" {
		action = "adopt existing skill (" + mode + ")"
	}
	p.Changes = append(p.Changes, Change{Path: destination, Action: action, exists: exists, adopted: adoption.CanonicalName != "",
		skill: &skillChange{source: source, sourceHash: sourceHash, beforeHash: current, mode: mode, adoptionHash: adoption.TreeSHA256}})
	return nil
}

func copySkill(source, destination string) error {
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if redirected(info) {
			return fmt.Errorf("symlink appeared while copying skill: %s", path)
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("special file in skill: %s", path)
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	return err
}
