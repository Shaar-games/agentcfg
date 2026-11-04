package syncer

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"agentcfg/internal/pathutil"
)

// The native entry and its content have separate identities. Never relax the
// physical-tree hasher: only scanner-proven top-level links may be adopted.
func adoptionEntryFingerprint(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if redirected(info) {
		return treeFingerprint(path)
	}
	return adoptionTreeFingerprint(path)
}

func (p *Plan) adoptedSkillSource(path, scannedSource string) (string, string, error) {
	if err := ensureNoSymlinkParents(p.base, filepath.Dir(path)); err != nil {
		return "", "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", "", err
	}
	if !redirected(info) {
		return path, "", nil
	}
	if scannedSource == "" {
		return "", "", fmt.Errorf("linked skill has no scanner-resolved source: %s", path)
	}
	source, err := pathutil.Resolve(path)
	if err != nil {
		return "", "", err
	}
	expected, err := scopePath(p.base, scannedSource)
	if err != nil || !sameAdoptionPath(source, expected) {
		return "", "", fmt.Errorf("native skill link target changed since discovery: %s", path)
	}
	relative, err := filepath.Rel(p.base, source)
	canonicalRelative, canonicalErr := filepath.Rel(filepath.Join(p.base, ".agentcfg"), source)
	if err != nil || !withinScope(relative) || canonicalErr == nil && withinScope(canonicalRelative) {
		return "", "", fmt.Errorf("native skill link target is outside the import scope: %s", path)
	}
	if err := ensureNoSymlinkParents(p.base, source); err != nil {
		return "", "", err
	}
	target, err := os.Readlink(path)
	if err != nil {
		return "", "", err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	target, err = scopePath(p.base, target)
	return source, target, err
}

func skillPathKey(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

// Detach a link before replacing/removing any native link it depends on.
// This also handles chains pointing into a shared skill that init retires.
func orderLinkedSkillAdoptions(changes []Change) ([]Change, error) {
	links := map[string]int{}
	for i, change := range changes {
		if change.skill != nil && change.skill.nativeLinkTarget != "" {
			links[skillPathKey(change.Path)] = i
		}
	}
	if len(links) == 0 {
		return changes, nil
	}
	dependents := map[int][]int{}
	for i, change := range changes {
		if change.skill == nil || change.skill.nativeLinkTarget == "" {
			continue
		}
		for target := change.skill.nativeLinkTarget; ; target = filepath.Dir(target) {
			if index, found := links[skillPathKey(target)]; found {
				dependents[index] = append(dependents[index], i)
				break
			}
			if filepath.Dir(target) == target {
				break
			}
		}
	}
	ordered := make([]Change, 0, len(changes))
	visited := map[int]int{}
	var visit func(int) error
	visit = func(i int) error {
		if visited[i] == 1 {
			return fmt.Errorf("cyclic native skill links: %s", changes[i].Path)
		}
		if visited[i] == 2 {
			return nil
		}
		visited[i] = 1
		for _, dependent := range dependents[i] {
			if err := visit(dependent); err != nil {
				return err
			}
		}
		visited[i] = 2
		ordered = append(ordered, changes[i])
		return nil
	}
	for i, change := range changes {
		if change.skill != nil && change.skill.nativeLinkTarget != "" {
			if err := visit(i); err != nil {
				return nil, err
			}
		}
	}
	for i, change := range changes {
		if visited[i] == 0 {
			ordered = append(ordered, change)
		}
	}
	return ordered, nil
}
