package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"agentcfg/internal/config"
	"agentcfg/internal/pathutil"
	"agentcfg/internal/syncer"
)

// skillImport is a read-only native source to be copied byte-for-byte into
// agentcfg's canonical user-level skill directory. It is not a project write.
type skillImport struct {
	Name       string
	Source     string
	TreeSHA256 string // scanner proof; empty only in standalone writer unit tests
}

// writeInitialSources atomically creates a fresh canonical source tree, after
// every selected skill has been copied into a private, adjacent staging home
// and config.Load validates the complete proposal. It never replaces an
// existing canonical tree, native harness file, or source SKILL.md.
func writeInitialSources(home string, settings config.Settings, servers map[string]map[string]any, skills []skillImport) error {
	absolute, err := filepath.Abs(home)
	if err != nil {
		return err
	}
	if err := verifyRealDirectory(absolute); err != nil {
		return fmt.Errorf("initialize user home: %w", err)
	}
	destination := filepath.Join(absolute, ".agentcfg")
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("canonical configuration %s already exists; refusing to overwrite or merge (use agentcfg config)", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	selected, err := config.ValidateUserHarnesses(settings.Harnesses)
	if err != nil {
		return err
	}
	settings.Harnesses = selected
	stagingHome, err := os.MkdirTemp(absolute, ".agentcfg-init-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stagingHome)
	if err := os.Chmod(stagingHome, 0o700); err != nil {
		return err
	}
	staged := filepath.Join(stagingHome, ".agentcfg")
	if err := os.Mkdir(staged, 0o700); err != nil {
		return err
	}
	for _, directory := range []string{"skills", "plugins"} {
		if err := os.Mkdir(filepath.Join(staged, directory), 0o700); err != nil {
			return err
		}
	}
	if err := writeNewJSON(filepath.Join(staged, "agentcfg.json"), settings); err != nil {
		return err
	}
	if servers == nil {
		servers = map[string]map[string]any{}
	}
	if err := writeNewJSON(filepath.Join(staged, "mcp.json"), map[string]any{"mcpServers": servers}); err != nil {
		return err
	}
	for _, skill := range skills {
		if !validImportName(skill.Name) {
			return fmt.Errorf("invalid imported skill name %q", skill.Name)
		}
		target := filepath.Join(staged, "skills", skill.Name)
		if err := copyNativeSkill(skill.Source, target); err != nil {
			return fmt.Errorf("import skill %q: %w", skill.Name, err)
		}
		// Validate the bytes actually staged against the read-only discovery
		// snapshot. A source replaced after the Huh prompt, or while copying,
		// must never become an unreviewed canonical skill.
		if skill.TreeSHA256 != "" {
			actual, err := syncer.SkillTreeSHA256(target)
			if err != nil || actual != skill.TreeSHA256 {
				return fmt.Errorf("import skill %q: staged bytes differ from the confirmed native discovery", skill.Name)
			}
		}
	}
	// Verify the exact configuration that would be visible from the final home
	// path without publishing it. Source MCP commands must not be executed.
	loaded, err := config.Load(stagingHome)
	if err != nil {
		return fmt.Errorf("generated canonical source is invalid: %w", err)
	}
	if len(loaded.Warnings) != 0 || len(loaded.Skills) != len(skills) || len(loaded.Servers) != len(servers) {
		return fmt.Errorf("generated canonical source failed validation (%d warning(s), %d/%d skills, %d/%d MCPs): %s",
			len(loaded.Warnings), len(loaded.Skills), len(skills), len(loaded.Servers), len(servers), strings.Join(loaded.Warnings, "; "))
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("canonical destination appeared during initialization; nothing overwritten")
		}
		return err
	}
	if err := os.Rename(staged, destination); err != nil {
		return fmt.Errorf("publish canonical sources: %w", err)
	}
	return nil
}

func writeNewJSON(path string, document any) (retErr error) {
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func validImportName(value string) bool {
	if len(value) == 0 || len(value) > 64 || strings.HasPrefix(value, "-") || strings.HasSuffix(value, "-") {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return !strings.Contains(value, "--")
}

func verifyRealDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || pathutil.IsRedirected(info) {
		return fmt.Errorf("expected a real, non-linked directory: %s", path)
	}
	return nil
}

// copyNativeSkill reads through an OS-anchored root and bounds size and file
// count. Every opened entry is checked against its Lstat identity. Nested
// links, junctions and special files cannot become imported assets.
func copyNativeSkill(source, dest string) error {
	if err := verifyRealDirectory(source); err != nil {
		return err
	}
	expected, err := os.Lstat(source)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(expected, opened) {
		return errors.New("native skill source changed while opening")
	}
	entries := 0
	var total int64
	var walk func(string) error
	walk = func(relative string) error {
		entries++
		if entries > 1024 {
			return errors.New("skill exceeds 1024 filesystem entries")
		}
		info, err := root.Lstat(relative)
		if err != nil {
			return err
		}
		if pathutil.IsRedirected(info) || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("linked or special native skill entry refused: %s", relative)
		}
		file, err := root.OpenFile(relative, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		opened, err := file.Stat()
		if err != nil || !os.SameFile(info, opened) {
			file.Close()
			return fmt.Errorf("native skill entry changed during open: %s", relative)
		}
		target := dest
		if relative != "." {
			target = filepath.Join(dest, relative)
		}
		switch {
		case info.IsDir():
			if err := os.Mkdir(target, 0o700); err != nil {
				file.Close()
				return err
			}
			children, err := file.ReadDir(1025)
			file.Close()
			if err != nil && err != io.EOF {
				return err
			}
			if len(children) > 1024 {
				return errors.New("native skill contains too many directory entries")
			}
			for _, child := range children {
				if err := walk(filepath.Join(relative, child.Name())); err != nil {
					return err
				}
			}
			return nil
		case info.Mode().IsRegular():
			total += info.Size()
			if total > 16<<20 || info.Size() > 4<<20 {
				file.Close()
				return errors.New("native skill exceeds 16 MiB total or 4 MiB per file")
			}
			return copyNewRegular(file, target, info)
		default:
			file.Close()
			return fmt.Errorf("native skill contains unsupported file type: %s", relative)
		}
	}
	if err := walk("."); err != nil {
		return err
	}
	if after, err := os.Lstat(source); err != nil || !os.SameFile(expected, after) {
		return errors.New("native skill source changed during import")
	}
	return nil
}

func copyNewRegular(in *os.File, dest string, original os.FileInfo) (retErr error) {
	defer in.Close()
	mode := os.FileMode(0o600)
	if original.Mode()&0o100 != 0 {
		mode |= 0o100
	}
	output, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			_ = os.Remove(dest)
		}
	}()
	count, copyErr := io.Copy(output, io.LimitReader(in, original.Size()+1))
	syncErr := output.Sync()
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if count != original.Size() {
		return errors.New("native skill file changed while copying")
	}
	return nil
}
