package syncer

import (
	"path/filepath"
	"sort"

	"agentcfg/internal/config"
)

// IsSharedSkillPath identifies direct children of the user-global discovery
// directory. It never resolves descendants or grants adoption authority.
func IsSharedSkillPath(home, path string) bool {
	return sameAdoptionPath(filepath.Dir(path), filepath.Join(home, ".agents", "skills"))
}

// Import locations include retired discovery roots. They are sources only;
// distribution always uses the current harness-specific destination.
func nativeSkillSource(home string, t target, path string) bool {
	if sameAdoptionPath(filepath.Dir(path), t.skills) {
		return true
	}
	return t.name == "codex" && IsSharedSkillPath(home, path) ||
		t.name == "opencode" && sameAdoptionPath(filepath.Dir(path), filepath.Join(home, ".opencode", "skills"))
}

func legacySkillPath(home, path string) bool {
	return IsSharedSkillPath(home, path) || sameAdoptionPath(filepath.Dir(path), filepath.Join(home, ".opencode", "skills"))
}

// Move only scanner-proven imports or unchanged entries owned by an older
// agentcfg out of the shared discovery root. Removal uses Apply's normal backup
// and rollback transaction, after the canonical copy has been validated.
func (p *Plan) planSharedSkills(c *config.Config, old manifest) error {
	paths := map[string]bool{}
	for path := range old.Skills {
		if IsSharedSkillPath(p.base, path) {
			paths[path] = true
		}
	}
	if p.adoption != nil {
		for path := range p.adoption.skills {
			if legacySkillPath(p.base, path) {
				paths[path] = true
			}
		}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	for _, path := range ordered {
		if err := ensureNoSymlinkParents(p.base, filepath.Dir(path)); err != nil {
			return err
		}
		if previous := old.Skills[path]; previous.Fingerprint != "" {
			if err := p.planSkill(path, config.Skill{}, false, "", previous); err != nil {
				return err
			}
		} else {
			if err := p.planImportedSkillRemoval(path, c); err != nil {
				return err
			}
		}
		p.Changes[len(p.Changes)-1].Action = "remove shared skill"
		p.migratesSharedSkills = true
	}
	return nil
}
