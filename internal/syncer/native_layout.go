package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentcfg/internal/fsstate"
)

type nativeLayoutEntry struct {
	Path         string `json:"path"`
	Original     string `json:"original"`
	Baseline     string `json:"baseline"`
	OriginalHash string `json:"originalHash"`
	BaselineHash string `json:"baselineHash"`
	Staged       string `json:"staged"`
}

func layoutOptions(follow bool) fsstate.Options {
	return fsstate.Options{FollowLinks: follow, Durable: true, MaxEntries: 100000, MaxBytes: 512 << 20, MaxFile: 64 << 20}
}

func pathUnder(parent, path string) bool {
	relative, err := filepath.Rel(parent, path)
	return err == nil && withinScope(relative)
}

func readInitPoint(home string) (initRestorePoint, []byte, error) {
	path := filepath.Join(home, ".agentcfg", initRestoreFile)
	if err := ensureNoSymlinkParents(home, path); err != nil {
		return initRestorePoint{}, nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return initRestorePoint{}, nil, err
	}
	var point initRestorePoint
	if len(data) > 16<<20 || json.Unmarshal(data, &point) != nil || point.Version != 1 {
		return point, nil, errors.New("invalid initialization restore point")
	}
	return point, data, nil
}

// MaterializeInitLinks snapshots all sources before detaching any alias. The
// syncer subsequently sees ordinary files even when native roots shared storage.
// Originals are moved, never recreated, so junctions need no extra privileges.
func MaterializeInitLinks(ctx context.Context, home string, harnesses []string, skills []SkillAdoption) error {
	options := func(follow bool) fsstate.Options {
		options := layoutOptions(follow)
		options.Context = ctx
		return options
	}
	base, err := normalizeScopeRoot(home)
	if err != nil {
		return err
	}
	var paths []string
	selected := map[string]bool{}
	for _, name := range harnesses {
		selected[name] = true
	}
	for _, target := range registry(base, "user") {
		if selected[target.name] {
			paths = append(paths, target.path, target.skills)
		}
	}
	for _, skill := range skills {
		paths = append(paths, skill.Path)
	}
	units := map[string]bool{}
	for _, path := range paths {
		path, err = scopePath(base, path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(base, path)
		current := base
		linked := false
		for _, part := range strings.Split(relative, string(filepath.Separator)) {
			current = filepath.Join(current, part)
			info, statErr := os.Lstat(current)
			if os.IsNotExist(statErr) {
				break
			}
			if statErr != nil {
				return statErr
			}
			if redirected(info) {
				units[current] = true
				linked = true
				break
			}
		}
		if linked {
			continue
		}
		if _, statErr := os.Lstat(path); os.IsNotExist(statErr) {
			continue
		} else if statErr != nil {
			return statErr
		}
		snapshot, err := fsstate.Capture(path, options(false))
		if err != nil {
			return err
		}
		if snapshot.Layout != emptyLayoutHash {
			units[path] = true
		}
	}
	var ordered []string
	for path := range units {
		ordered = append(ordered, path)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i]) != len(ordered[j]) {
			return len(ordered[i]) < len(ordered[j])
		}
		return ordered[i] < ordered[j]
	})
	roots := []string{}
	for _, path := range ordered {
		covered := false
		for _, root := range roots {
			if pathUnder(root, path) {
				covered = true
				break
			}
		}
		if !covered {
			roots = append(roots, path)
		}
	}
	if len(roots) == 0 {
		return nil
	}
	point, before, err := readInitPoint(base)
	if err != nil {
		return err
	}
	if len(point.Layout) != 0 {
		return errors.New("native link layout was already prepared")
	}
	for _, path := range roots {
		if err := ensureNoSymlinkParents(base, filepath.Dir(path)); err != nil {
			return err
		}
		relative, _ := filepath.Rel(base, path)
		folder := filepath.Join("restore", "layout", rawDigest([]byte(filepath.ToSlash(relative)))[:16])
		entry := nativeLayoutEntry{Path: filepath.ToSlash(relative), Original: filepath.ToSlash(filepath.Join(folder, "original")), Baseline: filepath.ToSlash(filepath.Join(folder, "baseline"))}
		original, err := fsstate.Capture(path, options(false))
		if err != nil {
			return err
		}
		entry.OriginalHash = original.State
		baseline := filepath.Join(base, ".agentcfg", filepath.FromSlash(entry.Baseline))
		if err := ensureNoSymlinkParents(base, baseline); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(baseline), 0700); err != nil {
			return err
		}
		if _, err := fsstate.Copy(path, baseline, options(true)); err != nil {
			return err
		}
		snapshot, err := fsstate.Capture(baseline, options(false))
		if err != nil {
			return err
		}
		entry.BaselineHash = snapshot.State
		temp, err := os.CreateTemp(filepath.Dir(path), ".agentcfg-layout-*")
		if err != nil {
			return err
		}
		staged := temp.Name()
		if err := temp.Close(); err != nil {
			return err
		}
		if err := os.Remove(staged); err != nil {
			return err
		}
		stagedRelative, err := filepath.Rel(base, staged)
		if err != nil || !withinScope(stagedRelative) {
			return errors.New("native staging path escapes user home")
		}
		entry.Staged = filepath.ToSlash(stagedRelative)
		point.Layout = append(point.Layout, entry)
	}
	data, err := json.MarshalIndent(point, "", "  ")
	if err != nil {
		return err
	}
	marker := filepath.Join(base, ".agentcfg", initRestoreFile)
	if err := atomicWriteChecked(marker, append(data, '\n'), 0600, func() error { return fileUnchanged(marker, before, true) }); err != nil {
		return err
	}
	// All expanded baselines exist before the first rename, including mutually
	// shared roots. Every rename is confined to a verified physical parent.
	for _, entry := range point.Layout {
		path := filepath.Join(base, filepath.FromSlash(entry.Path))
		if err := ensureNoSymlinkParents(base, filepath.Dir(path)); err != nil {
			return err
		}
		current, err := fsstate.Capture(path, options(false))
		if err != nil || current.State != entry.OriginalHash {
			return fmt.Errorf("native layout changed since preview: %s", path)
		}
		baseline := filepath.Join(base, ".agentcfg", filepath.FromSlash(entry.Baseline))
		expanded, err := fsstate.Capture(path, options(true))
		if err != nil {
			return err
		}
		saved, err := fsstate.Capture(baseline, options(false))
		if err != nil || expanded.Content != saved.Content || expanded.Fingerprint != saved.Fingerprint {
			return fmt.Errorf("native layout contents changed: %s", path)
		}
		staged := filepath.Join(base, filepath.FromSlash(entry.Staged))
		if _, err := fsstate.Copy(baseline, staged, options(true)); err != nil {
			return err
		}
		original := filepath.Join(base, ".agentcfg", filepath.FromSlash(entry.Original))
		if err := os.Rename(path, original); err != nil {
			return err
		}
		if err := os.Rename(staged, path); err != nil {
			_ = os.Rename(original, path)
			return err
		}
	}
	return nil
}

const emptyLayoutHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func layoutCovers(home string, entries []nativeLayoutEntry, path string) bool {
	for _, entry := range entries {
		if pathUnder(filepath.Join(home, filepath.FromSlash(entry.Path)), path) {
			return true
		}
	}
	return false
}

func validateLayoutEntry(home string, entry nativeLayoutEntry) (string, string, string, error) {
	path := filepath.Join(home, filepath.FromSlash(entry.Path))
	if filepath.IsAbs(entry.Path) || !withinScope(filepath.FromSlash(entry.Path)) || entry.Path == "." {
		return "", "", "", errors.New("invalid native layout target")
	}
	allowed := false
	for _, target := range registry(home, "user") {
		if pathUnder(path, target.path) || pathUnder(path, target.skills) || pathUnder(target.skills, path) {
			allowed = true
		}
	}
	if pathUnder(filepath.Join(home, ".agents", "skills"), path) || pathUnder(path, filepath.Join(home, ".agents", "skills")) || pathUnder(filepath.Join(home, ".opencode", "skills"), path) {
		allowed = true
	}
	if !allowed {
		return "", "", "", errors.New("native layout target is not a user harness location")
	}
	staged := filepath.Join(home, filepath.FromSlash(entry.Staged))
	if filepath.IsAbs(entry.Staged) || !withinScope(filepath.FromSlash(entry.Staged)) || filepath.Dir(staged) != filepath.Dir(path) || !strings.HasPrefix(filepath.Base(staged), ".agentcfg-layout-") {
		return "", "", "", errors.New("invalid native layout staging path")
	}
	folder := filepath.Join("restore", "layout", rawDigest([]byte(entry.Path))[:16])
	if filepath.FromSlash(entry.Original) != filepath.Join(folder, "original") || filepath.FromSlash(entry.Baseline) != filepath.Join(folder, "baseline") {
		return "", "", "", errors.New("invalid native layout backup path")
	}
	original := filepath.Join(home, ".agentcfg", filepath.FromSlash(entry.Original))
	baseline := filepath.Join(home, ".agentcfg", filepath.FromSlash(entry.Baseline))
	if err := ensureNoSymlinkParents(home, filepath.Dir(path)); err != nil {
		return "", "", "", err
	}
	if err := ensureNoSymlinkParents(home, baseline); err != nil {
		return "", "", "", err
	}
	return path, original, baseline, nil
}

func layoutMask(home, path string, native []restoreEntry) fsstate.Options {
	var writes []string
	for _, entry := range native {
		write := filepath.Join(home, filepath.FromSlash(entry.Path))
		if pathUnder(path, write) {
			relative, _ := filepath.Rel(path, write)
			writes = append(writes, relative)
		}
	}
	options := layoutOptions(false)
	options.Skip = func(relative string) bool {
		for _, write := range writes {
			if pathUnder(write, relative) {
				return true
			}
		}
		return false
	}
	options.OmitDirectory = func(relative string) bool {
		for _, write := range writes {
			if pathUnder(relative, write) {
				return true
			}
		}
		return false
	}
	return options
}

func layoutAlreadyRestored(home string, entry nativeLayoutEntry) bool {
	path := filepath.Join(home, filepath.FromSlash(entry.Path))
	snapshot, err := fsstate.Capture(path, layoutOptions(false))
	return err == nil && snapshot.State == entry.OriginalHash
}

func validateNativeLayout(home string, point initRestorePoint) error {
	var paths, staging []string
	for _, entry := range point.Layout {
		path, original, baseline, err := validateLayoutEntry(home, entry)
		if err != nil {
			return err
		}
		stage := filepath.Join(home, filepath.FromSlash(entry.Staged))
		for i, previous := range paths {
			if pathUnder(previous, path) || pathUnder(path, previous) || sameAdoptionPath(staging[i], stage) {
				return errors.New("native layout contains overlapping targets or staging paths")
			}
		}
		paths, staging = append(paths, path), append(staging, stage)
		if err := checkLayoutStaging(home, entry, false); err != nil {
			return err
		}
		if layoutAlreadyRestored(home, entry) {
			continue
		}
		backup, err := fsstate.Capture(original, layoutOptions(false))
		if err != nil || backup.State != entry.OriginalHash {
			return errors.New("original native layout backup changed or is missing")
		}
		saved, err := fsstate.Capture(baseline, layoutOptions(false))
		if err != nil || saved.State != entry.BaselineHash {
			return errors.New("expanded native layout snapshot changed")
		}
		options := layoutMask(home, path, point.Native)
		a, err := fsstate.Capture(path, options)
		if os.IsNotExist(err) && len(point.Native) == 0 {
			continue
		}
		if err != nil {
			return err
		}
		b, err := fsstate.Capture(baseline, options)
		if err != nil || a.State != b.State {
			return fmt.Errorf("unmanaged contents of native layout changed since init: %s", path)
		}
	}
	return nil
}

func restoreNativeLayout(home string, point initRestorePoint) error {
	if err := validateNativeLayout(home, point); err != nil {
		return err
	}
	var undos []func() error
	rollback := func(cause error) error {
		for i := len(undos) - 1; i >= 0; i-- {
			cause = errors.Join(cause, undos[i]())
		}
		return cause
	}
	for _, entry := range point.Layout {
		if err := checkLayoutStaging(home, entry, true); err != nil {
			return rollback(err)
		}
		if layoutAlreadyRestored(home, entry) {
			continue
		}
		path, original, baseline, err := validateLayoutEntry(home, entry)
		if err != nil {
			return rollback(err)
		}
		if _, err := os.Lstat(path); os.IsNotExist(err) && len(point.Native) == 0 {
			if err := os.Rename(original, path); err != nil {
				return rollback(err)
			}
			undos = append(undos, func() error { return os.Rename(path, original) })
			continue
		}
		saved, err := fsstate.Capture(baseline, layoutOptions(false))
		if err != nil {
			return rollback(err)
		}
		// Sync may have created empty parent directories for newly installed
		// skills. Only remove those known write ancestors absent from baseline.
		directories := map[string]bool{}
		for _, dir := range saved.Directories {
			directories[dir] = true
		}
		var cleanup []string
		for _, change := range point.Native {
			current := filepath.Dir(filepath.Join(home, filepath.FromSlash(change.Path)))
			for pathUnder(path, current) && !sameAdoptionPath(path, current) {
				relative, _ := filepath.Rel(path, current)
				if !directories[relative] {
					cleanup = append(cleanup, current)
				}
				current = filepath.Dir(current)
			}
		}
		sort.Slice(cleanup, func(i, j int) bool { return len(cleanup[i]) > len(cleanup[j]) })
		for _, dir := range cleanup {
			if err := ensureNoSymlinkParents(home, dir); err != nil {
				return rollback(err)
			}
			if children, err := os.ReadDir(dir); err == nil && len(children) == 0 {
				if err := os.Remove(dir); err != nil {
					return rollback(err)
				}
			}
		}
		current, err := fsstate.Capture(path, layoutOptions(false))
		if err != nil || current.State != entry.BaselineHash {
			return rollback(fmt.Errorf("native layout does not match restored baseline: %s", path))
		}
		discard := filepath.Join(filepath.Dir(original), "restored-copy")
		if _, err := os.Lstat(discard); !os.IsNotExist(err) {
			return rollback(errors.New("native layout restore staging already exists"))
		}
		if err := os.Rename(path, discard); err != nil {
			return rollback(err)
		}
		installed := false
		undos = append(undos, func() error {
			if installed {
				if err := os.Rename(path, original); err != nil {
					return err
				}
			}
			return os.Rename(discard, path)
		})
		if err := os.Rename(original, path); err != nil {
			return rollback(err)
		}
		installed = true
	}
	return nil
}

func checkLayoutStaging(home string, entry nativeLayoutEntry, remove bool) error {
	staged := filepath.Join(home, filepath.FromSlash(entry.Staged))
	if err := ensureNoSymlinkParents(home, filepath.Dir(staged)); err != nil {
		return err
	}
	snapshot, err := fsstate.Capture(staged, layoutOptions(false))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if snapshot.Layout != emptyLayoutHash {
		return errors.New("native staging entry was replaced with a link")
	}
	baseline := filepath.Join(home, ".agentcfg", filepath.FromSlash(entry.Baseline))
	saved, err := fsstate.Capture(baseline, layoutOptions(false))
	if err != nil || saved.State != entry.BaselineHash {
		return errors.New("native layout baseline changed")
	}
	directories := map[string]bool{}
	for _, dir := range saved.Directories {
		directories[dir] = true
	}
	for _, dir := range snapshot.Directories {
		if !directories[dir] {
			return errors.New("unrecognized directory in native staging")
		}
	}
	for file, hash := range snapshot.Files {
		if saved.Files[file] != hash {
			return errors.New("native staging contains externally modified files")
		}
	}
	if remove {
		return os.RemoveAll(staged)
	}
	return nil
}
