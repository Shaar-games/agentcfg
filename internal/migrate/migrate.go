// Package migrate provides an explicit, non-destructive transition from the
// legacy agentmux user directory to the agentcfg user directory.
package migrate

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"agentcfg/internal/auth"
	"agentcfg/internal/pathutil"
)

const (
	legacyName = ".agentmux"
	newName    = ".agentcfg"
)

// Report describes the canonical data copied. Native harness configurations,
// running brokers, and installed binaries are deliberately outside its scope.
type Report struct {
	Legacy      string
	Destination string
	Directories int
	Files       int
	Bytes       int64
	Paths       []string // relative destination paths, never source contents
	Warnings    []string
}

type entry struct {
	rel  string
	info fs.FileInfo
}

// Preview performs filesystem validation without creating files, even when
// the old store includes credential records.
func Preview(home string) (Report, error) {
	report, _, err := inspect(home)
	return report, err
}

func inspect(home string) (Report, []entry, error) {
	home, err := filepath.Abs(home)
	if err != nil {
		return Report{}, nil, err
	}
	report := Report{
		Legacy: filepath.Join(home, legacyName), Destination: filepath.Join(home, newName),
	}
	if err := requireDirectory(home); err != nil {
		return report, nil, fmt.Errorf("home: %w", err)
	}
	if _, err := os.Lstat(report.Destination); err == nil {
		return report, nil, fmt.Errorf("destination %s already exists; resolve the conflict manually (nothing was overwritten)", report.Destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return report, nil, err
	}
	if err := requireDirectory(report.Legacy); err != nil {
		return report, nil, fmt.Errorf("legacy configuration: %w", err)
	}
	var entries []entry
	names := map[string]string{}
	err = filepath.WalkDir(report.Legacy, func(path string, dirEntry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(report.Legacy, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if pathutil.IsRedirected(info) || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("linked/reparse source cannot be migrated automatically: %s", path)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular source cannot be migrated automatically: %s", path)
		}
		destRel := destinationRel(rel)
		folded := strings.ToLower(filepath.ToSlash(destRel))
		if prior, ok := names[folded]; ok {
			return fmt.Errorf("source naming collision: %s and %s both map to %s", prior, rel, destRel)
		}
		names[folded] = rel
		entries = append(entries, entry{rel: rel, info: info})
		report.Paths = append(report.Paths, destRel)
		if info.IsDir() {
			report.Directories++
		} else {
			report.Files++
			report.Bytes += info.Size()
		}
		return nil
	})
	if err != nil {
		return report, nil, err
	}
	report.Warnings = []string{
		"Previous ~/.agentmux data and OAuth ciphertext remain intact.",
		"Native harness MCP proxies may still invoke agentmux; run agentcfg sync and review --apply after migration.",
		"The legacy agentmux broker and executable are not stopped or removed.",
	}
	return report, entries, nil
}

func destinationRel(rel string) string {
	if rel == "agentmux.json" {
		return "agentcfg.json"
	}
	return rel
}

func requireDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || pathutil.IsRedirected(info) {
		return fmt.Errorf("%s must be a real directory (links/junctions are refused)", path)
	}
	return nil
}

// Apply makes an entirely new canonical tree in an unpublished private staging
// directory and promotes it only after all copied files have been verified.
// The old tree is never renamed, deleted, rewritten or used as a backup target.
func Apply(home string) (Report, error) {
	report, entries, err := inspect(home)
	if err != nil {
		return report, err
	}
	stage, err := os.MkdirTemp(filepath.Dir(report.Destination), ".agentcfg-migration-")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0o700); err != nil {
		return report, err
	}
	if err := auth.ProtectUserDirectory(stage); err != nil {
		return report, fmt.Errorf("restrict staging directory: %w", err)
	}
	for _, e := range entries {
		source := filepath.Join(report.Legacy, e.rel)
		destination := filepath.Join(stage, destinationRel(e.rel))
		if e.info.IsDir() {
			if err := os.Mkdir(destination, 0o700); err != nil {
				return report, err
			}
			continue
		}
		if err := copyVerified(source, destination, e.info); err != nil {
			return report, fmt.Errorf("copy %s: %w", e.rel, err)
		}
	}
	// A legacy process might have changed a source after it was copied. Refuse
	// promotion instead of publishing an inconsistent snapshot.
	latest, current, err := inspect(home)
	if err != nil {
		return report, err
	}
	if latest.Files != report.Files || latest.Directories != report.Directories || latest.Bytes != report.Bytes ||
		len(current) != len(entries) {
		return report, errors.New("legacy configuration changed during migration; no new configuration published")
	}
	for i, e := range entries {
		if e.rel != current[i].rel || !os.SameFile(e.info, current[i].info) ||
			e.info.Size() != current[i].info.Size() || !e.info.ModTime().Equal(current[i].info.ModTime()) {
			return report, fmt.Errorf("source changed during migration: %s", e.rel)
		}
		if !e.info.IsDir() {
			sourceHash, err := hashFile(filepath.Join(report.Legacy, e.rel))
			if err != nil {
				return report, err
			}
			targetHash, err := hashFile(filepath.Join(stage, destinationRel(e.rel)))
			if err != nil {
				return report, err
			}
			if sourceHash != targetHash {
				return report, fmt.Errorf("source changed during migration: %s", e.rel)
			}
		}
	}
	// Check again immediately before promoting; the new canonical tree never
	// replaces an existing target. Race-free exchange requires OS-specific APIs;
	// no other process should create ~/.agentcfg during this explicit migration.
	if _, err := os.Lstat(report.Destination); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return report, errors.New("destination appeared during migration; refusing to overwrite")
		}
		return report, err
	}
	if err := os.Rename(stage, report.Destination); err != nil {
		return report, fmt.Errorf("publish new canonical directory: %w", err)
	}
	return report, nil
}

func copyVerified(source, destination string, expected fs.FileInfo) (retErr error) {
	current, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !os.SameFile(current, expected) || !current.Mode().IsRegular() || pathutil.IsRedirected(current) ||
		current.Size() != expected.Size() || !current.ModTime().Equal(expected.ModTime()) {
		return errors.New("source changed since preview")
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil || !os.SameFile(current, opened) {
		return errors.New("source changed during open")
	}
	mode := os.FileMode(0o600)
	if expected.Mode()&0o111 != 0 {
		mode = 0o700
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			_ = os.Remove(destination)
		}
	}()
	digest := sha256.New()
	count, copyErr := io.Copy(io.MultiWriter(out, digest), in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if count != expected.Size() {
		return errors.New("source length changed while copying")
	}
	copied, err := hashFile(destination)
	if err != nil {
		return err
	}
	if copied != fmt.Sprintf("%x", digest.Sum(nil)) {
		return errors.New("copied file checksum mismatch")
	}
	return nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
