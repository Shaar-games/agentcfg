package syncer

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agentcfg/internal/config"
	"agentcfg/internal/pathutil"
)

type pathAliasLink func(*testing.T, string, string)

type pathAliasFixture struct {
	adoptionFixture
	home, alias string
}

func newPathAliasFixture(t *testing.T, link pathAliasLink) pathAliasFixture {
	t.Helper()
	f := newAdoptionFixture(t, "cursor", true)
	home, err := pathutil.Resolve(f.opts.Home)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range registry(home, "user") {
		if candidate.name == "cursor" {
			f.target = candidate
		}
	}
	f.canonicalSkill = filepath.Join(home, ".agentcfg", "skills", "review")
	f.nativeSkill = filepath.Join(f.target.skills, "review")
	f.opts.Root, f.opts.Home = home, home
	f.opts.Binary = filepath.Join(home, "bin", "agentcfg.exe")
	f.opts.AdoptMCP[0].Path = f.target.path
	f.opts.AdoptSkills[0].Path = f.nativeSkill
	parent := filepath.Join(t.TempDir(), "parent-alias")
	link(t, filepath.Dir(home), parent)
	alias := filepath.Join(parent, filepath.Base(home))
	physicalInfo, err := os.Lstat(home)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Lstat(alias)
	if err != nil || redirected(aliasInfo) || !os.SameFile(physicalInfo, aliasInfo) {
		t.Fatalf("fixture must alias only the root's parent: %v", err)
	}
	f.cfg, err = config.Load(alias)
	if err != nil {
		t.Fatal(err)
	}
	return pathAliasFixture{adoptionFixture: f, home: home, alias: alias}
}

func pathAliasSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func (f pathAliasFixture) spelling(t *testing.T, root, path string) string {
	t.Helper()
	relative, err := filepath.Rel(f.home, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatalf("test path is outside its temporary home: %s (%v)", path, err)
	}
	return filepath.Join(root, relative)
}

func (f pathAliasFixture) ordinaryOptions() Options {
	opts := f.opts
	opts.AdoptExisting, opts.AdoptMCP, opts.AdoptSkills = false, nil, nil
	return opts
}

func pathAliasApply(t *testing.T, f pathAliasFixture) *Plan {
	t.Helper()
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	assertAdoptionBytes(t, f.target.path, f.before)
	assertNoAdoptionManifest(t, f.home)
	adopted := 0
	for _, change := range plan.Changes {
		if change.adopted {
			adopted++
		}
	}
	if adopted != 2 {
		t.Fatalf("want both MCP and skill adoption, got %d adopted changes", adopted)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestPathAliasParentSymlinkAdoption(t *testing.T) {
	testPathAliasParentAdoption(t, pathAliasSymlink)
}

func testPathAliasParentAdoption(t *testing.T, link pathAliasLink) {
	t.Helper()
	for _, test := range []struct {
		name                                       string
		homeAlias, rootAlias, mcpAlias, skillAlias bool
	}{
		{"alias roots with physical proofs", true, true, false, false},
		{"physical roots with alias proofs", false, false, true, true},
		{"mixed MCP proof", true, false, true, false},
		{"mixed skill proof", false, true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newPathAliasFixture(t, link)
			if test.homeAlias {
				f.opts.Home = f.alias
			}
			if test.rootAlias {
				f.opts.Root = f.alias
			}
			if test.mcpAlias {
				f.opts.AdoptMCP[0].Path = f.spelling(t, f.alias, f.target.path)
			}
			if test.skillAlias {
				f.opts.AdoptSkills[0].Path = f.spelling(t, f.alias, f.nativeSkill)
			}
			plan := pathAliasApply(t, f)
			if plan.state.Configs[f.target.path]["echo"] == "" || plan.state.Skills[f.nativeSkill].Fingerprint == "" {
				t.Fatal("adoption did not record ownership at physical destination keys")
			}
			source, err := os.ReadFile(filepath.Join(f.canonicalSkill, "SKILL.md"))
			if err != nil {
				t.Fatal(err)
			}
			assertAdoptionBytes(t, filepath.Join(f.nativeSkill, "SKILL.md"), source)
			for _, roots := range [][2]string{{f.alias, f.home}, {f.home, f.alias}} {
				opts := f.ordinaryOptions()
				opts.Root, opts.Home = roots[0], roots[1]
				next, err := BuildPlan(f.cfg, opts)
				if err != nil {
					t.Fatalf("ordinary sync lost aliased ownership: %v", err)
				}
				if len(next.Changes) != 0 {
					t.Fatalf("ordinary sync after adoption is not idempotent: %+v", next.Changes)
				}
				if err := next.Apply(); err != nil {
					t.Fatalf("ordinary no-op apply through an alias failed: %v", err)
				}
			}
		})
	}
}

func TestPathAliasInteriorSymlinksRefused(t *testing.T) {
	testPathAliasInteriorLinks(t, pathAliasSymlink)
}

func testPathAliasInteriorLinks(t *testing.T, link pathAliasLink) {
	t.Helper()
	for _, kind := range []string{"config directory", "native skill", "skill parent", "canonical skill"} {
		t.Run(kind, func(t *testing.T) {
			f := newPathAliasFixture(t, link)
			path := filepath.Dir(f.target.path)
			switch kind {
			case "native skill":
				path = f.nativeSkill
			case "skill parent":
				path = filepath.Dir(f.nativeSkill)
			case "canonical skill":
				path = f.canonicalSkill
			}
			preserved := filepath.Join(t.TempDir(), "preserved")
			if err := os.Rename(path, preserved); err != nil {
				t.Fatal(err)
			}
			link(t, preserved, path)
			spelled := f.spelling(t, f.alias, path)
			rebound, err := scopePath(f.home, spelled)
			if err != nil || rebound != path {
				t.Fatalf("rebasing erased or followed an interior link: got %q, %v; want %q", rebound, err, path)
			}
			if err := ensureNoSymlinkParents(f.home, spelled); err == nil {
				t.Fatal("scope validation accepted an interior link through a parent alias")
			}
			f.opts.Home, f.opts.Root = f.alias, f.alias
			if _, err := BuildPlan(f.cfg, f.opts); err == nil {
				t.Fatal("adoption followed a link inside the temporary home")
			}
			assertAdoptionBytes(t, f.target.path, f.before)
			assertNoAdoptionManifest(t, f.home)
		})
	}
}

func TestPathAliasNativeConfigFileSymlinkRefused(t *testing.T) {
	f := newPathAliasFixture(t, pathAliasSymlink)
	preserved := filepath.Join(t.TempDir(), "native.json")
	if err := os.Rename(f.target.path, preserved); err != nil {
		t.Fatal(err)
	}
	pathAliasSymlink(t, preserved, f.target.path)
	f.opts.Home, f.opts.Root = f.alias, f.alias
	f.opts.AdoptMCP[0].Path = f.spelling(t, f.alias, f.target.path)
	if _, err := BuildPlan(f.cfg, f.opts); err == nil {
		t.Fatal("aliased proof hid a symlink at the native config file")
	}
	assertAdoptionBytes(t, preserved, f.before)
	assertNoAdoptionManifest(t, f.home)
}

func TestPathAliasLinkedRootUsesPhysicalIdentity(t *testing.T) {
	testPathAliasLinkedRoot(t, pathAliasSymlink)
}

func testPathAliasLinkedRoot(t *testing.T, link pathAliasLink) {
	t.Helper()
	f := newPathAliasFixture(t, link)
	pathAliasApply(t, f)
	linkedRoot := filepath.Join(t.TempDir(), "linked-home")
	link(t, f.home, linkedRoot)
	if normalized, err := normalizeScopeRoot(linkedRoot); err != nil || normalized != f.home {
		t.Fatalf("linked root did not resolve to its physical identity: %s %v", normalized, err)
	}
	for _, adopt := range []bool{false, true} {
		opts := f.opts
		if !adopt {
			opts = f.ordinaryOptions()
		}
		opts.Root, opts.Home = linkedRoot, linkedRoot
		plan, err := BuildPlan(f.cfg, opts)
		if adopt {
			if err == nil || !strings.Contains(err.Error(), "managed") {
				t.Fatalf("alias bypassed existing ownership checks: %v", err)
			}
		} else if err != nil || len(plan.Changes) != 0 {
			t.Fatalf("linked root lost managed ownership: %v", err)
		}
	}
}

func TestPathAliasRootReentrySymlinkRefused(t *testing.T) {
	testPathAliasRootReentry(t, pathAliasSymlink)
}

func testPathAliasRootReentry(t *testing.T, link pathAliasLink) {
	t.Helper()
	for _, aliasSpelling := range []bool{false, true} {
		for _, proof := range []string{"MCP", "skill"} {
			t.Run(proof+map[bool]string{false: " physical", true: " alias"}[aliasSpelling], func(t *testing.T) {
				f := newPathAliasFixture(t, link)
				link(t, filepath.Dir(f.home), filepath.Join(f.home, "return-to-parent"))
				spelling := f.home
				if aliasSpelling {
					spelling = f.alias
				}
				reentry := filepath.Join(spelling, "return-to-parent", filepath.Base(f.home))
				target := f.target.path
				if proof == "skill" {
					target = f.nativeSkill
				}
				suspect := f.spelling(t, reentry, target)
				expected := f.spelling(t, filepath.Join(f.home, "return-to-parent", filepath.Base(f.home)), target)
				rebound, err := scopePath(f.home, suspect)
				if err != nil || rebound != expected {
					t.Fatalf("rebasing hid the in-home root re-entry: got %q, %v; want %q", rebound, err, expected)
				}
				if err := ensureNoSymlinkParents(f.home, suspect); err == nil {
					t.Fatal("scope validation erased a forbidden in-home root re-entry")
				}
				f.opts.Home, f.opts.Root = spelling, spelling
				if proof == "MCP" {
					f.opts.AdoptMCP[0].Path = suspect
				} else {
					f.opts.AdoptSkills[0].Path = suspect
				}
				if _, err := BuildPlan(f.cfg, f.opts); err == nil {
					t.Fatal("adoption accepted a proof traversing a link back into its own home")
				}
				assertAdoptionBytes(t, f.target.path, f.before)
				assertNoAdoptionManifest(t, f.home)
			})
		}
	}
}

func TestPathAliasMissingDescendantsRemainLexical(t *testing.T) {
	f := newPathAliasFixture(t, pathAliasSymlink)
	physical := filepath.Join(f.home, "not-created", "nested", "config.json")
	spelled := f.spelling(t, f.alias, physical)
	got, err := scopePath(f.home, spelled)
	if err != nil || got != physical {
		t.Fatalf("cannot rebase an absent descendant: %q, %v", got, err)
	}
	if err := ensureNoSymlinkParents(f.alias, physical); err != nil {
		t.Fatalf("physical descendant of an aliased scope was refused: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.home, "not-created")); !os.IsNotExist(err) {
		t.Fatalf("scope checks created a missing directory: %v", err)
	}
}

func TestPathAliasManifestMapsPreserveValues(t *testing.T) {
	f := newPathAliasFixture(t, pathAliasSymlink)
	configPath := f.target.path
	blockPath := filepath.Join(f.home, ".codex", "config.toml")
	instructionsPath := filepath.Join(f.home, ".claude", "CLAUDE.md")
	old := freshManifest()
	old.Configs[f.spelling(t, f.alias, configPath)] = map[string]string{"echo": "config hash"}
	old.Blocks[f.spelling(t, f.alias, blockPath)] = "block hash"
	old.Skills[f.spelling(t, f.alias, f.nativeSkill)] = skillRecord{
		Source: f.canonicalSkill, SourceHash: "source hash", Fingerprint: "tree:ownership hash", Mode: "copy",
	}
	old.Instructions[f.spelling(t, f.alias, instructionsPath)] = "instructions hash"
	want := freshManifest()
	want.Configs[configPath] = old.Configs[f.spelling(t, f.alias, configPath)]
	want.Blocks[blockPath] = "block hash"
	want.Skills[f.nativeSkill] = old.Skills[f.spelling(t, f.alias, f.nativeSkill)]
	want.Instructions[instructionsPath] = "instructions hash"
	got, err := normalizeManifestPaths(old, f.home)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest path normalization changed ownership values: got %+v, error %v; want %+v", got, err, want)
	}
}

func TestPathAliasManifestDuplicateKeysRefused(t *testing.T) {
	for _, kind := range []string{"config", "block", "skill", "instructions"} {
		t.Run(kind, func(t *testing.T) {
			f := newPathAliasFixture(t, pathAliasSymlink)
			physical, alias := f.target.path, f.spelling(t, f.alias, f.target.path)
			old := freshManifest()
			switch kind {
			case "config":
				old.Configs[physical], old.Configs[alias] = map[string]string{"echo": "same hash"}, map[string]string{"echo": "same hash"}
			case "block":
				old.Blocks[physical], old.Blocks[alias] = "same hash", "same hash"
			case "skill":
				record := skillRecord{Fingerprint: "tree:same hash"}
				old.Skills[physical], old.Skills[alias] = record, record
			case "instructions":
				old.Instructions[physical], old.Instructions[alias] = "same hash", "same hash"
			}
			if _, err := normalizeManifestPaths(old, f.home); err == nil || !strings.Contains(err.Error(), "duplicate aliases") {
				t.Fatalf("duplicate ownership aliases were accepted for %s: %v", kind, err)
			}
		})
	}
}

func pathAliasManifest(t *testing.T, f pathAliasFixture) ([]byte, manifest) {
	t.Helper()
	path := filepath.Join(f.home, ".agentcfg", "sync-user.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := freshManifest()
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	aliased := freshManifest()
	for path, value := range old.Configs {
		aliased.Configs[f.spelling(t, f.alias, path)] = value
	}
	for path, value := range old.Skills {
		aliased.Skills[f.spelling(t, f.alias, path)] = value
	}
	for path, value := range old.Blocks {
		aliased.Blocks[f.spelling(t, f.alias, path)] = value
	}
	for path, value := range old.Instructions {
		aliased.Instructions[f.spelling(t, f.alias, path)] = value
	}
	data, err = json.MarshalIndent(aliased, "", "\t")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	writeTestFile(t, path, string(data))
	return data, old
}

func TestPathAliasExistingManifestRemainsIdempotent(t *testing.T) {
	f := newPathAliasFixture(t, pathAliasSymlink)
	pathAliasApply(t, f)
	before, old := pathAliasManifest(t, f)
	opts := f.ordinaryOptions()
	opts.Home, opts.Root = f.alias, f.home
	plan, err := BuildPlan(f.cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("rebased ownership introduced changes: %+v", plan.Changes)
	}
	if !reflect.DeepEqual(plan.state, old) {
		t.Fatalf("rebased ownership changed the managed state: got %+v; want %+v", plan.state, old)
	}
	if !bytes.Equal(plan.stateBefore, before) {
		t.Fatal("normalization replaced the original manifest bytes used by the apply guard")
	}
	if err := plan.Apply(); err != nil {
		t.Fatalf("apply rejected unchanged raw manifest bytes after rebasing: %v", err)
	}
}

func TestPathAliasExistingManifestStillRejectsDrift(t *testing.T) {
	for _, kind := range []string{"MCP", "skill", "manifest after preview"} {
		t.Run(kind, func(t *testing.T) {
			f := newPathAliasFixture(t, pathAliasSymlink)
			pathAliasApply(t, f)
			before, _ := pathAliasManifest(t, f)
			opts := f.ordinaryOptions()
			opts.Home, opts.Root = f.home, f.alias
			if kind == "manifest after preview" {
				plan, err := BuildPlan(f.cfg, opts)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(f.home, ".agentcfg", "sync-user.json")
				edited := append(append([]byte(nil), before...), '\n')
				writeTestFile(t, path, string(edited))
				if err := plan.Apply(); err == nil || !strings.Contains(err.Error(), "changed since planning") {
					t.Fatalf("aliased manifest lost its concurrent-edit guard: %v", err)
				}
				assertAdoptionBytes(t, path, edited)
				return
			}
			path, edited, reason := f.target.path, f.before, "edited or removed"
			if kind == "skill" {
				path = filepath.Join(f.nativeSkill, "references", "rules.txt")
				edited = []byte("A manual change after adoption.\n")
				reason = "modified or removed"
			}
			writeTestFile(t, path, string(edited))
			if _, err := BuildPlan(f.cfg, opts); err == nil || !strings.Contains(err.Error(), reason) {
				t.Fatalf("alias rebasing lost the %s managed-drift check: %v", kind, err)
			}
			assertAdoptionBytes(t, path, edited)
			assertAdoptionBytes(t, filepath.Join(f.home, ".agentcfg", "sync-user.json"), before)
		})
	}
}
