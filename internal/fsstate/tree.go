// Package fsstate snapshots user-selected filesystem trees without executing
// their contents. Reads may follow links; writes always create physical copies.
package fsstate

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentcfg/internal/pathutil"
)

type Options struct {
	Context           context.Context
	FollowLinks       bool
	MetadataOnly      bool
	Durable           bool
	Boundary          string // optional physical read boundary; empty permits explicit external links
	MaxEntries        int
	MaxBytes, MaxFile int64
	Skip              func(string) bool
	OmitDirectory     func(string) bool
}

type Snapshot struct {
	Content, Layout, State, Fingerprint string
	Entries                             int
	Bytes                               int64
	Directories                         []string
	Files                               map[string]string
}

func Capture(path string, options Options) (*Snapshot, error) { return walk(path, "", options) }
func Copy(path, destination string, options Options) (*Snapshot, error) {
	if options.MetadataOnly {
		return nil, errors.New("metadata-only snapshots cannot be copied")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		if err == nil {
			return nil, fmt.Errorf("copy destination already exists: %s", destination)
		}
		return nil, err
	}
	return walk(path, destination, options)
}

func walk(path, destination string, options Options) (*Snapshot, error) {
	if options.MaxEntries == 0 {
		options.MaxEntries = 10000
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = 64 << 20
	}
	if options.MaxFile == 0 {
		options.MaxFile = 16 << 20
	}
	boundary := ""
	if options.Boundary != "" {
		var err error
		boundary, err = pathutil.Resolve(options.Boundary)
		if err != nil {
			return nil, err
		}
	}
	snapshot := &Snapshot{Files: map[string]string{}}
	content, layout, state := sha256.New(), sha256.New(), sha256.New()
	var ancestors []os.FileInfo
	var visit func(string, string, int) error
	visit = func(source, relative string, depth int) error {
		if options.Context != nil {
			if err := options.Context.Err(); err != nil {
				return err
			}
		}
		if options.Skip != nil && options.Skip(relative) {
			return nil
		}
		snapshot.Entries++
		if depth > 64 || snapshot.Entries > options.MaxEntries {
			return errors.New("filesystem tree exceeds depth or entry limit")
		}
		entry, err := os.Lstat(source)
		if err != nil {
			return err
		}
		original := entry
		linked := pathutil.IsRedirected(entry)
		if linked {
			target, err := os.Readlink(source)
			if err != nil {
				return err
			}
			record(layout, 'l', relative)
			record(layout, 't', target)
			fmt.Fprintf(layout, "%d;", entry.Mode().Type())
			if !options.FollowLinks {
				if destination != "" {
					return fmt.Errorf("copying links requires FollowLinks: %s", source)
				}
				after, err := os.Lstat(source)
				if err != nil || !os.SameFile(original, after) || original.Mode() != after.Mode() {
					return fmt.Errorf("link replaced during snapshot: %s", source)
				}
				current, err := os.Readlink(source)
				if err != nil || current != target {
					return fmt.Errorf("link changed during snapshot: %s", source)
				}
				record(content, 'l', relative)
				record(content, 't', target)
				record(state, 'l', relative)
				record(state, 't', target)
				return nil
			}
		}
		// Descendants use their resolved parent's physical spelling. Resolve
		// only the root or an actual link; repeatedly resolving every regular
		// asset is expensive on Windows and adds no stronger identity check.
		resolved := source
		if depth == 0 || linked {
			resolved, err = pathutil.Resolve(source)
			if err != nil {
				return err
			}
		}
		if boundary != "" {
			rel, err := filepath.Rel(boundary, resolved)
			if err != nil || !within(rel) {
				return fmt.Errorf("link target escapes read boundary: %s", source)
			}
		}
		if linked {
			record(layout, 'r', resolved)
		}
		entry, err = os.Lstat(resolved)
		if err != nil {
			return err
		}
		if pathutil.IsRedirected(entry) {
			return fmt.Errorf("entry changed while resolving: %s", source)
		}
		target := destination
		if relative != "." {
			target = filepath.Join(destination, relative)
		}
		if entry.IsDir() {
			for _, ancestor := range ancestors {
				if os.SameFile(ancestor, entry) {
					return fmt.Errorf("directory link cycle: %s", source)
				}
			}
			ancestors = append(ancestors, entry)
			defer func() { ancestors = ancestors[:len(ancestors)-1] }()
			if options.OmitDirectory == nil || !options.OmitDirectory(relative) {
				record(content, 'd', relative)
				record(state, 'd', relative)
			}
			snapshot.Directories = append(snapshot.Directories, relative)
			if destination != "" {
				if err := os.Mkdir(target, entry.Mode().Perm()|0700); err != nil {
					return err
				}
				if err := os.Chmod(target, entry.Mode().Perm()|0700); err != nil {
					return err
				}
			}
			children, err := ReadDir(resolved, options.MaxEntries-snapshot.Entries)
			if err != nil {
				return err
			}
			sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
			for _, child := range children {
				if err := visit(filepath.Join(resolved, child.Name()), filepath.Join(relative, child.Name()), depth+1); err != nil {
					return err
				}
			}
		} else {
			if !entry.Mode().IsRegular() {
				return fmt.Errorf("unsupported special filesystem entry: %s", source)
			}
			if entry.Size() > options.MaxFile || snapshot.Bytes+entry.Size() > options.MaxBytes {
				return errors.New("filesystem tree exceeds byte limit")
			}
			if options.MetadataOnly {
				snapshot.Bytes += entry.Size()
				return nil
			}
			file, err := os.OpenFile(resolved, os.O_RDONLY|nonblockRead, 0)
			if err != nil {
				return err
			}
			opened, err := file.Stat()
			if err != nil || !opened.Mode().IsRegular() || !os.SameFile(entry, opened) {
				file.Close()
				return fmt.Errorf("file changed while opening: %s", source)
			}
			h := sha256.New()
			var output *os.File
			var outputIdentity os.FileInfo
			var writer io.Writer = h
			if destination != "" {
				output, err = os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, entry.Mode().Perm())
				if err != nil {
					file.Close()
					return err
				}
				writer = io.MultiWriter(h, output)
				outputIdentity, _ = output.Stat()
			}
			var reader io.Reader = file
			if options.Context != nil {
				reader = contextReader{options.Context, file}
			}
			count, copyErr := io.Copy(writer, io.LimitReader(reader, options.MaxFile+1))
			after, statErr := file.Stat()
			closeErr := file.Close()
			if output != nil {
				if options.Durable {
					closeErr = errors.Join(closeErr, output.Sync())
				}
				closeErr = errors.Join(closeErr, output.Chmod(entry.Mode().Perm()))
				closeErr = errors.Join(closeErr, output.Close())
			}
			if copyErr != nil || closeErr != nil || statErr != nil || count != entry.Size() || !os.SameFile(entry, after) || after.Size() != entry.Size() || !after.ModTime().Equal(entry.ModTime()) {
				if outputIdentity != nil {
					if current, err := os.Lstat(target); err == nil && os.SameFile(outputIdentity, current) {
						_ = os.Remove(target)
					}
				}
				return errors.Join(fmt.Errorf("file changed while reading: %s", source), copyErr, closeErr, statErr)
			}
			snapshot.Bytes += count
			record(content, 'f', relative)
			content.Write(h.Sum(nil))
			record(state, 'f', relative)
			state.Write(h.Sum(nil))
			fmt.Fprintf(state, "%d;", entry.Mode().Perm()&0111)
			snapshot.Files[filepath.ToSlash(relative)] = hex.EncodeToString(h.Sum(nil))
		}
		after, err := os.Lstat(source)
		if err != nil || !os.SameFile(original, after) || original.Mode() != after.Mode() {
			return fmt.Errorf("entry replaced during snapshot: %s", source)
		}
		if linked {
			current, err := pathutil.Resolve(source)
			if err != nil || current != resolved {
				return fmt.Errorf("link changed during snapshot: %s", source)
			}
		}
		return nil
	}
	err := visit(path, ".", 0)
	snapshot.Content = hex.EncodeToString(content.Sum(nil))
	snapshot.Layout = hex.EncodeToString(layout.Sum(nil))
	snapshot.Fingerprint = hex.EncodeToString(state.Sum(nil))
	state.Write(layout.Sum(nil))
	snapshot.State = hex.EncodeToString(state.Sum(nil))
	return snapshot, err
}

func record(h hash.Hash, kind byte, name string) {
	name = filepath.ToSlash(name)
	h.Write([]byte{kind})
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(name)))
	h.Write(size[:])
	h.Write([]byte(name))
}

func within(relative string) bool {
	return relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func ReadDir(path string, limit int) ([]os.DirEntry, error) {
	resolved, err := pathutil.Resolve(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(resolved)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("expected a readable directory: %s", path)
	}
	file, err := os.Open(resolved)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("directory changed while opening")
	}
	entries, err := file.ReadDir(limit + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > limit {
		return nil, errors.New("directory exceeds entry limit")
	}
	current, err := pathutil.Resolve(path)
	if err != nil || current != resolved {
		return nil, errors.New("directory link changed while reading")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func ReadFile(path, boundary string, limit int64) ([]byte, error) {
	snapshot, err := Capture(path, Options{FollowLinks: true, Boundary: boundary, MaxFile: limit, MaxBytes: limit})
	if err != nil {
		return nil, err
	}
	resolved, err := pathutil.Resolve(path)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(resolved, os.O_RDONLY|nonblockRead, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("file exceeds byte limit")
	}
	h := sha256.Sum256(data)
	if hex.EncodeToString(h[:]) != snapshot.Files["."] {
		return nil, errors.New("file changed after snapshot")
	}
	return data, nil
}

type contextReader struct {
	context context.Context
	reader  io.Reader
}

func (r contextReader) Read(data []byte) (int, error) {
	if err := r.context.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}
