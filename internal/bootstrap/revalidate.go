package bootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

// SourceSHA256 returns the raw native MCP file hash captured during Discover.
// It does not read the file again; it is suitable for a subsequent consent proof.
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
// private source fingerprints and cannot validate selected server imports.
func (p *Plan) Revalidate() error {
	if p == nil || !filepath.IsAbs(p.Home) {
		return errors.New("invalid discovery plan")
	}
	root, err := os.OpenRoot(p.Home)
	if err != nil {
		return errors.New("discovery home changed or is unreadable")
	}
	defer root.Close()
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
			relative, err := anchoredPath(root, source)
			if err != nil {
				return errors.New("selected MCP source changed or escapes home")
			}
			data, err := readFile(root, relative, maxConfigBytes, true)
			if err != nil || sha256.Sum256(data) != expected {
				return errors.New("selected MCP source changed since discovery")
			}
			checked[source] = true
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
