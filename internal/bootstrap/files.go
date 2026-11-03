package bootstrap

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"unicode/utf8"
)

const (
	maxConfigBytes = 1 << 20
	maxTreeEntries = 10000
	maxAssetBytes  = 16 << 20
	maxTreeBytes   = 64 << 20
)

// Resolve once for Windows junction support, then open relative to the anchored
// root. A concurrent link replacement cannot redirect the subsequent read out
// of that root. Absolute in-home aliases are supported at the discovery edge.
func anchoredPath(root *os.Root, source string) (string, error) {
	resolved, err := contained(root.Name(), source)
	if err != nil {
		return "", err
	}
	return filepath.Rel(root.Name(), resolved)
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
