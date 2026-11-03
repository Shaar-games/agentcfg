// Package bootstrap discovers existing user-level harness configuration without
// executing commands or changing files. The caller owns confirmation and writes.
package bootstrap

import "agentcfg/internal/config"

// Plan is a reviewable import inventory. Discover never applies it. Selected
// fields are initial suggestions; the confirmation UI may change them.
type Plan struct {
	Home         string
	Harnesses    []Harness
	Servers      []ServerCandidate
	Skills       []SkillCandidate
	Warnings     []Warning
	sourceHashes map[string][32]byte
	budget       *scanBudget
}

type Harness struct {
	Name     string
	Detected bool
	Selected bool
	Evidence []string
}

// Origin preserves the pairing between a native path and its harness. Sources
// and Harnesses are unions after deduplication and must not be cross-joined.
type Origin struct {
	Harness string
	Source  string
}

// Server contains only definitions that can be represented canonically.
// Sensitive or unsupported native entries are omitted and reported in Warnings.
// Conflicting names remain separate, unselected candidates with unique IDs.
type ServerCandidate struct {
	ID              string
	Name            string
	Source          string
	Sources         []string
	Harnesses       []string
	Origins         []Origin
	Server          config.Server
	Selected        bool
	Warnings        []string
	CanonicalSHA256 string
}

// Source is an absolute skill directory. SKILL.md and its assets are never
// rewritten. Fingerprint identifies the validated tree; the caller must
// revalidate after confirmation before copying from a possibly changed source.
type SkillCandidate struct {
	ID          string
	Name        string
	Source      string
	Sources     []string
	Harnesses   []string
	Origins     []Origin
	Selected    bool
	Warnings    []string
	Fingerprint string
	TreeSHA256  string
}

type Warning struct {
	Harness string
	Source  string
	Message string
}
