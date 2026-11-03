package bootstrap

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"unicode/utf8"

	"agentcfg/internal/pathutil"
)

const (
	maxConfigBytes = 1 << 20
	maxTreeEntries = 10000
	maxAssetBytes  = 16 << 20
	maxTreeBytes   = 64 << 20
)

// Resolve both namespaces, but verify that the named root still identifies the
// directory held by os.Root. Re-resolving root.Name alone would let a retargeted
// alias supply paths relative to a different directory than the one being read.
func anchoredPath(root *os.Root, source string) (string, error) {
	physicalRoot, err := anchoredRootPath(root)
	if err != nil {
		return "", err
	}
	resolved, err := contained(physicalRoot, source)
	if err != nil {
		return "", err
	}
	relative, err := relativeWithin(physicalRoot, resolved)
	if err != nil {
		return "", err
	}
	if _, err := anchoredRootPath(root); err != nil {
		return "", err
	}
	return relative, nil
}

func anchoredRootPath(root *os.Root) (string, error) {
	opened, err := root.Stat(".")
	if err != nil {
		return "", err
	}
	physical, err := pathutil.Resolve(root.Name())
	if err != nil {
		return "", err
	}
	named, err := os.Stat(physical)
	if err != nil || !os.SameFile(opened, named) {
		return "", errors.New("discovery root name no longer identifies the opened directory")
	}
	return physical, nil
}

// Capture only spellings observed for this read. In particular, a later lookup
// must not follow a new alias or rehash a changed file to manufacture a proof.
func (p *Plan) rememberSourceHash(native, physicalNative, physicalTarget string, value [32]byte) bool {
	if p.sourceHashes == nil {
		p.sourceHashes = map[string][32]byte{}
	}
	for _, path := range []string{native, physicalNative, physicalTarget} {
		if p.ambiguousSourceHashes[path] {
			return false
		}
		if previous, ok := p.sourceHashes[path]; ok && previous != value {
			delete(p.sourceHashes, path)
			if p.ambiguousSourceHashes == nil {
				p.ambiguousSourceHashes = map[string]bool{}
			}
			p.ambiguousSourceHashes[path] = true
			return false
		}
	}
	for _, path := range []string{native, physicalNative, physicalTarget} {
		p.sourceHashes[path] = value
	}
	return true
}

func captureSourceHash(root *os.Root, plan *Plan, source, relative string, value [32]byte) error {
	physicalRoot, err := anchoredRootPath(root)
	if err != nil {
		return err
	}
	nativeRelative, err := relativeWithin(root.Name(), source)
	if err != nil {
		return err
	}
	if !plan.rememberSourceHash(source, filepath.Join(physicalRoot, nativeRelative), filepath.Join(physicalRoot, relative), value) {
		return errors.New("native source changed between discovery captures")
	}
	return nil
}

func readFile(root *os.Root, path string, limit int64, text bool) ([]byte, error) {
	info, err := root.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("file is nonregular or exceeds the read limit")
	}
	file, err := root.OpenFile(path, os.O_RDONLY|nonblockRead, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() > limit {
		return nil, errors.New("file changed or is nonregular")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return data, errors.New("file is unreadable or exceeds the read limit")
	}
	if text && !utf8.Valid(data) {
		return data, errors.New("file is not UTF-8")
	}
	after, err := file.Stat()
	if err != nil || after.Size() != int64(len(data)) || after.Size() != opened.Size() ||
		after.Mode() != opened.Mode() || !after.ModTime().Equal(opened.ModTime()) {
		return data, errors.New("file changed while being read")
	}
	return data, nil
}

func readDir(root *os.Root, path string) ([]os.DirEntry, error) {
	file, err := root.OpenFile(path, os.O_RDONLY|nonblockRead, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries, err := file.ReadDir(maxTreeEntries + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > maxTreeEntries {
		return nil, errors.New("directory exceeds the entry limit")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}
