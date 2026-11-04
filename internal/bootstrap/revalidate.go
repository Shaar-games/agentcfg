package bootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"

	"agentcfg/internal/fsstate"
	"agentcfg/internal/pathutil"
)

// SameInventory compares discoveries, not scanner counters or volatile stat
// metadata (directory times, Windows stat caches). Identity and raw source
// fingerprints remain part of the comparison; callers also Revalidate before
// importing so an actual source edit or home replacement is still refused.
func (p *Plan) SameInventory(other *Plan) bool {
	if p == nil || other == nil || p.homeIdentity == nil || other.homeIdentity == nil || !os.SameFile(p.homeIdentity, other.homeIdentity) {
		return false
	}
	return p.Home == other.Home && reflect.DeepEqual(p.Harnesses, other.Harnesses) &&
		reflect.DeepEqual(p.Servers, other.Servers) && reflect.DeepEqual(p.Skills, other.Skills) &&
		reflect.DeepEqual(p.Warnings, other.Warnings) && reflect.DeepEqual(p.sourceHashes, other.sourceHashes) &&
		reflect.DeepEqual(p.ambiguousSourceHashes, other.ambiguousSourceHashes)
}

// SourceSHA256 returns the raw MCP file hash captured during Discover. Both the
// native spelling and its root-resolved/target-resolved spellings are captured.
// Lookup is exact: it never resolves new aliases, cleans parent traversals, or
// reads file contents again. Freshness is checked separately by Revalidate.
func (p *Plan) SourceSHA256(path string) (string, bool) {
	if p == nil {
		return "", false
	}
	value, ok := p.sourceHashes[path]
	if !ok {
		return "", false
	}
	return hex.EncodeToString(value[:]), true
}

// Revalidate checks the native MCP files and complete skill trees backing the
// currently selected candidates. It neither applies the plan nor freezes disk:
// callers still need containment-safe reads during the subsequent atomic copy.
// Keep the original Plan returned by Discover; reconstructed plans lack the
// private home identity and source fingerprints required for validation.
func (p *Plan) Revalidate() error {
	if p == nil || !filepath.IsAbs(p.Home) {
		return errors.New("invalid discovery plan")
	}
	root, err := os.OpenRoot(p.Home)
	if err != nil {
		return errors.New("discovery home changed or is unreadable")
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || p.homeIdentity == nil || !os.SameFile(p.homeIdentity, opened) {
		return errors.New("discovery home no longer identifies the scanned directory")
	}
	if _, err := anchoredRootPath(root); err != nil {
		return errors.New("discovery home changed or is unreadable")
	}
	checked := map[string]bool{}
	for _, candidate := range p.Servers {
		if !candidate.Selected {
			continue
		}
		if len(candidate.Sources) == 0 {
			return errors.New("selected server has no discovery source")
		}
		for _, source := range candidate.Sources {
			if checked[source] {
				continue
			}
			expected, ok := p.sourceHashes[source]
			if !ok {
				return errors.New("selected server has no discovery fingerprint")
			}
			data, err := fsstate.ReadFile(source, "", maxConfigBytes)
			if err != nil || sha256.Sum256(data) != expected {
				return errors.New("selected MCP source changed since discovery")
			}
			checked[source] = true
		}
		for _, origin := range candidate.Origins {
			resolved, err := pathutil.Resolve(origin.Source)
			if err != nil || origin.Target != "" && resolved != origin.Target {
				return errors.New("native MCP link target changed since discovery")
			}
		}
	}
	for _, candidate := range p.Skills {
		if !candidate.Selected {
			continue
		}
		if len(candidate.Sources) == 0 {
			return errors.New("selected skill has no discovery source")
		}
		sources := append([]string(nil), candidate.Sources...)
		for _, origin := range candidate.Origins {
			if origin.Target != "" {
				resolved, err := pathutil.Resolve(origin.Source)
				if err != nil || resolved != origin.Target {
					return errors.New("native skill link target changed since discovery")
				}
			}
			if origin.Layout != "" {
				_, _, layout, _, err := inspectSkill(root, origin.Source, candidate.Name, nil)
				if err != nil || layout != origin.Layout {
					return errors.New("native skill link layout changed since discovery")
				}
			}
			sources = union(sources, []string{origin.Source})
		}
		for _, source := range sources {
			copy := candidate
			copy.Source = source
			if err := verifySkill(root, copy); err != nil {
				return err
			}
		}
	}
	return nil
}
