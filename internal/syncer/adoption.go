package syncer

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"agentcfg/internal/config"
	"agentcfg/internal/pathutil"

	"github.com/tailscale/hujson"
)

// MCPAdoption authorizes one scanner-observed native MCP entry, after explicit
// user consent. Path is an absolute native configuration path. Name is its
// native key, and CanonicalName is the corresponding imported server ID.
// Both hashes must be captured by the scanner, before preview/confirmation.
type MCPAdoption struct {
	Harness, Path, Name, CanonicalName string
	FileSHA256                         string
	CanonicalSHA256                    string
}

// SkillAdoption authorizes one existing native skill directory. TreeSHA256 is
// captured at discovery using SkillTreeSHA256; the canonical tree must still
// have exactly the same file bytes and relative names when planning/applying.
type SkillAdoption struct {
	Harness, Path, CanonicalName, TreeSHA256 string
}

// ServerSHA256 fingerprints a resolved upstream definition. Routing policy and
// display/source identity are excluded so an imported entry can be renamed or
// assigned to harnesses without changing its upstream identity.
func ServerSHA256(server config.Server) string {
	server.Name, server.Source, server.Sharing = "", "", ""
	server.Harnesses = nil
	return digest(server)
}

// SkillTreeSHA256 hashes a physical skill directory, including empty directories
// and every regular file. It ignores permissions, rejects links/reparse points
// and special files, and frames records so file contents cannot impersonate tree
// structure. It neither writes files nor follows links outside the tree.
// Trees are limited to 10,000 entries, 64 levels, 16 MiB per file and 64 MiB
// in total, matching the native import scanner's bounds.
func SkillTreeSHA256(path string) (string, error) {
	content, _, err := skillTreeHashes(path, "")
	return content, err
}

// The second hash retains the existing ownership-manifest format. Adoption
// computes both hashes in one bounded, anchored pass; ordinary sync semantics
// and previously written manifest hashes remain compatible.
func adoptionTreeFingerprint(path string) (string, error) {
	_, fingerprint, err := skillTreeHashes(path, "")
	return fingerprint, err
}

func skillTreeHashes(path, destination string) (string, string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	parent := filepath.Dir(path)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return "", "", err
	}
	if redirected(parentInfo) || !parentInfo.IsDir() {
		return "", "", fmt.Errorf("skill adoption requires a physical parent directory: %s", parent)
	}
	if !os.SameFile(parentInfo, parentInfo) {
		return "", "", fmt.Errorf("cannot capture the skill parent identity: %s", parent)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", "", err
	}
	if redirected(info) || !info.IsDir() {
		return "", "", fmt.Errorf("skill adoption requires a physical directory: %s", path)
	}
	if !os.SameFile(info, info) {
		return "", "", fmt.Errorf("cannot capture the skill directory identity: %s", path)
	}
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		return "", "", err
	}
	defer parentRoot.Close()
	openedParent, err := parentRoot.Stat(".")
	if err != nil || !os.SameFile(parentInfo, openedParent) {
		return "", "", fmt.Errorf("skill parent changed while opening: %s", parent)
	}
	root, err := parentRoot.OpenRoot(filepath.Base(path))
	if err != nil {
		return "", "", err
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return "", "", fmt.Errorf("skill directory changed while opening: %s", path)
	}
	hash, legacy := sha256.New(), sha256.New()
	count, total := 0, int64(0)
	if err := hashSkillTree(root, ".", destination, hash, legacy, 0, &count, &total); err != nil {
		return "", "", err
	}
	after, err := os.Lstat(path)
	if err != nil || redirected(after) || !os.SameFile(info, after) {
		return "", "", fmt.Errorf("skill directory changed while hashing: %s", path)
	}
	parentAfter, err := os.Lstat(parent)
	if err != nil || redirected(parentAfter) || !os.SameFile(parentInfo, parentAfter) {
		return "", "", fmt.Errorf("skill parent changed while hashing: %s", parent)
	}
	return hex.EncodeToString(hash.Sum(nil)), "tree:" + hex.EncodeToString(legacy.Sum(nil)), nil
}

const (
	maxAdoptionTreeEntries = 10000
	maxAdoptionAssetBytes  = 16 << 20
	maxAdoptionTreeBytes   = 64 << 20
)

func hashSkillTree(root *os.Root, relative, destination string, h, legacy hash.Hash, depth int, count *int, total *int64) error {
	*count++
	if depth > 64 || *count > maxAdoptionTreeEntries {
		return errors.New("skill adoption tree exceeds the entry/depth limit")
	}
	info, err := root.Lstat(relative)
	if err != nil {
		return err
	}
	if redirected(info) || (!info.IsDir() && !info.Mode().IsRegular()) {
		return fmt.Errorf("skill adoption refuses links or special files: %s", relative)
	}
	if !os.SameFile(info, info) {
		return fmt.Errorf("cannot capture the skill entry identity: %s", relative)
	}
	file, err := root.OpenFile(relative, os.O_RDONLY|adoptionNonblockRead, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || redirected(opened) {
		return fmt.Errorf("skill changed while hashing: %s", relative)
	}
	kind := byte('f')
	if info.IsDir() {
		kind = 'd'
	}
	_, _ = h.Write([]byte{kind})
	name := []byte(filepath.ToSlash(relative))
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(name)))
	_, _ = h.Write(length[:])
	_, _ = h.Write(name)
	_, _ = fmt.Fprintf(legacy, "%s\x00%d\x00", filepath.ToSlash(relative), info.Mode())
	if info.IsDir() {
		if destination != "" {
			if err := os.MkdirAll(filepath.Join(destination, relative), info.Mode().Perm()|0o700); err != nil {
				return err
			}
		}
		entries, err := file.ReadDir(maxAdoptionTreeEntries + 1)
		if err != nil && err != io.EOF {
			return err
		}
		if len(entries) > maxAdoptionTreeEntries {
			return errors.New("skill adoption directory exceeds the entry limit")
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if err := hashSkillTree(root, filepath.Join(relative, entry.Name()), destination, h, legacy, depth+1, count, total); err != nil {
				return err
			}
		}
		return nil
	}
	limit := int64(maxAdoptionAssetBytes)
	if remaining := int64(maxAdoptionTreeBytes) - *total; remaining < limit {
		limit = remaining
	}
	if !opened.Mode().IsRegular() || opened.Size() > limit {
		return fmt.Errorf("skill adoption file exceeds the byte limit: %s", relative)
	}
	content := sha256.New()
	writers := []io.Writer{content, legacy}
	var output *os.File
	if destination != "" {
		output, err = os.OpenFile(filepath.Join(destination, relative), os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		writers = append(writers, output)
	}
	read, err := io.Copy(io.MultiWriter(writers...), io.LimitReader(file, limit+1))
	if output != nil {
		err = errors.Join(err, output.Close())
	}
	if err != nil {
		return err
	}
	if read > limit {
		return fmt.Errorf("skill adoption tree exceeds the byte limit: %s", relative)
	}
	*total += read
	_, _ = h.Write(content.Sum(nil))
	return nil
}

type adoptionState struct {
	home     string
	homeInfo os.FileInfo
	mcp      map[string]map[string]MCPAdoption
	skills   map[string]SkillAdoption
	targets  map[string]target
}

func sameAdoptionPath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func validSHA256(value string) bool {
	data, err := hex.DecodeString(value)
	return err == nil && len(data) == sha256.Size
}

func prepareAdoption(c *config.Config, opts Options, all []target, selected map[string]bool, old manifest) (*adoptionState, error) {
	if !opts.AdoptExisting {
		if len(opts.AdoptMCP) != 0 || len(opts.AdoptSkills) != 0 {
			return nil, errors.New("adoption proofs require explicit AdoptExisting consent")
		}
		return nil, nil
	}
	if opts.Scope != "user" {
		return nil, errors.New("adoption is supported only for explicitly confirmed user-scope init --apply")
	}
	home, err := pathutil.Resolve(opts.Home)
	if err != nil {
		return nil, err
	}
	for _, path := range []string{opts.Home, opts.Root, c.Root} {
		resolved, err := pathutil.Resolve(path)
		if err != nil || !sameAdoptionPath(resolved, home) || !sameAdoptionPath(path, resolved) {
			return nil, errors.New("adoption requires the canonical root and target home to be the same physical directory")
		}
	}
	homeInfo, err := os.Lstat(home)
	if err != nil || redirected(homeInfo) || !homeInfo.IsDir() {
		return nil, errors.New("adoption requires a physical target home")
	}
	// Windows FileInfo may load its file ID lazily. Force that read while the
	// preview's path still exists, before retaining the identity for Apply.
	if !os.SameFile(homeInfo, homeInfo) {
		return nil, errors.New("cannot capture the adoption home identity")
	}
	a := &adoptionState{home: home, homeInfo: homeInfo, mcp: map[string]map[string]MCPAdoption{}, skills: map[string]SkillAdoption{}, targets: map[string]target{}}
	targets := map[string]target{}
	for _, t := range all {
		targets[t.name] = t
	}
	canonicalTargets := map[string]map[string]bool{}
	for _, proof := range opts.AdoptMCP {
		t, ok := targets[proof.Harness]
		if !ok || !selected[proof.Harness] {
			return nil, fmt.Errorf("MCP adoption names an unselected or unknown harness %q", proof.Harness)
		}
		if !filepath.IsAbs(proof.Path) || !sameAdoptionPath(proof.Path, t.path) {
			return nil, fmt.Errorf("MCP adoption path is not the active native configuration for %s: %s", proof.Harness, proof.Path)
		}
		if t.toml {
			return nil, fmt.Errorf("native TOML MCP adoption is not supported for %s; import without --apply and resolve the native tables explicitly before syncing", t.name)
		}
		server, ok := c.Servers[proof.CanonicalName]
		if proof.Name == "" || !ok || !config.Allows(server.Harnesses, t.name) {
			return nil, fmt.Errorf("MCP adoption %q has no enabled canonical server %q for %s", proof.Name, proof.CanonicalName, t.name)
		}
		if !validSHA256(proof.FileSHA256) || !validSHA256(proof.CanonicalSHA256) ||
			!strings.EqualFold(proof.CanonicalSHA256, ServerSHA256(server)) {
			return nil, fmt.Errorf("MCP adoption proof for %q is missing or its canonical definition changed since discovery", proof.Name)
		}
		if _, owned := old.Configs[t.path][proof.Name]; owned {
			return nil, fmt.Errorf("MCP %q is already managed; adoption cannot bypass ownership checks", proof.Name)
		}
		if _, owned := old.Configs[t.path][proof.CanonicalName]; owned {
			return nil, fmt.Errorf("canonical MCP %q is already managed; adoption cannot replace its ownership", proof.CanonicalName)
		}
		if proof.Name != proof.CanonicalName {
			if other, exists := c.Servers[proof.Name]; exists && config.Allows(other.Harnesses, t.name) {
				return nil, fmt.Errorf("native MCP %q is also desired as another canonical server; ambiguous adoption rename", proof.Name)
			}
		}
		if a.mcp[t.path] == nil {
			a.mcp[t.path] = map[string]MCPAdoption{}
			canonicalTargets[t.path] = map[string]bool{}
		}
		if _, duplicate := a.mcp[t.path][proof.Name]; duplicate || canonicalTargets[t.path][proof.CanonicalName] {
			return nil, fmt.Errorf("duplicate or conflicting MCP adoption proof for %q", proof.Name)
		}
		proof.Path = t.path
		a.mcp[t.path][proof.Name] = proof
		canonicalTargets[t.path][proof.CanonicalName] = true
		a.targets[t.name] = t
	}
	for _, proof := range opts.AdoptSkills {
		t, ok := targets[proof.Harness]
		if !ok || !selected[proof.Harness] {
			return nil, fmt.Errorf("skill adoption names an unselected or unknown harness %q", proof.Harness)
		}
		var canonical *config.Skill
		for i := range c.Skills {
			if c.Skills[i].Name == proof.CanonicalName {
				canonical = &c.Skills[i]
				break
			}
		}
		if canonical == nil || !config.Allows(canonical.Harnesses, t.name) {
			return nil, fmt.Errorf("skill adoption has no enabled canonical skill %q for %s", proof.CanonicalName, t.name)
		}
		destination := filepath.Join(t.skills, filepath.Base(canonical.Path))
		if !filepath.IsAbs(proof.Path) || !sameAdoptionPath(proof.Path, destination) || sameAdoptionPath(destination, canonical.Path) {
			return nil, fmt.Errorf("skill adoption path is not the generated native destination for %q: %s", proof.CanonicalName, proof.Path)
		}
		if !validSHA256(proof.TreeSHA256) {
			return nil, fmt.Errorf("skill adoption proof is missing a tree SHA-256 for %q", proof.CanonicalName)
		}
		if _, duplicate := a.skills[destination]; duplicate {
			return nil, fmt.Errorf("duplicate skill adoption proof for %s", destination)
		}
		if old.Skills[destination].Fingerprint != "" {
			return nil, fmt.Errorf("skill %q is already managed; adoption cannot bypass ownership checks", proof.CanonicalName)
		}
		if err := ensureNoSymlinkParents(home, canonical.Path); err != nil {
			return nil, err
		}
		proof.Path = destination
		a.skills[destination] = proof
		a.targets[t.name] = t
	}
	return a, nil
}

// recheckAdoptionScope also detects a newly-created preferred OpenCode JSON
// file invalidating a preview that selected its JSONC alternative.
func (p *Plan) recheckAdoptionScope() error {
	if p.adoption == nil {
		return nil
	}
	info, err := os.Lstat(p.base)
	if err != nil || redirected(info) || !os.SameFile(info, p.adoption.homeInfo) {
		return errors.New("adoption home changed since planning")
	}
	resolved, err := pathutil.Resolve(p.base)
	if err != nil || !sameAdoptionPath(resolved, p.adoption.home) {
		return errors.New("adoption home changed since planning")
	}
	for _, current := range registry(p.base, "user") {
		if expected, ok := p.adoption.targets[current.name]; ok &&
			(!sameAdoptionPath(current.path, expected.path) || !sameAdoptionPath(current.skills, expected.skills)) {
			return fmt.Errorf("native destination for %s changed since discovery; refresh preview", current.name)
		}
	}
	return nil
}

func (p *Plan) adoptedMCP(t target, before []byte, previous map[string]string) (map[string]string, bool, error) {
	if p.adoption == nil || len(p.adoption.mcp[t.path]) == 0 {
		return previous, false, nil
	}
	proofs := p.adoption.mcp[t.path]
	document, err := uniqueJSONObject(before)
	if err != nil {
		return nil, false, fmt.Errorf("cannot adopt ambiguous native JSON: %w", err)
	}
	entries, ok := document[t.key].(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("adoption requires an existing %s object", t.key)
	}
	approved := make(map[string]string, len(previous)+len(proofs))
	for name, hash := range previous {
		approved[name] = hash
	}
	names := make([]string, 0, len(proofs))
	for name := range proofs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		proof := proofs[name]
		if !strings.EqualFold(proof.FileSHA256, rawDigest(before)) {
			return nil, false, fmt.Errorf("native MCP configuration changed since discovery: %s", t.path)
		}
		entry, ok := entries[name].(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("scanner-approved native MCP %q is absent or not an object", name)
		}
		approved[name] = digest(entry)
		p.Warnings = append(p.Warnings, fmt.Sprintf("explicitly adopt native MCP %q as %q for %s (%s)", name, proof.CanonicalName, t.name, t.path))
	}
	return approved, true, nil
}

// Native adoption snapshots are read through an anchored home, with regular
// file/identity checks and a bound. In particular, a substituted FIFO must not
// block either planning or the comparison immediately before replacement.
func (p *Plan) readAdoptionFile(path string) ([]byte, bool, error) {
	if err := ensureNoSymlinkParents(p.base, path); err != nil {
		return nil, false, err
	}
	baseInfo, err := os.Lstat(p.base)
	if err != nil || redirected(baseInfo) || !baseInfo.IsDir() {
		return nil, false, errors.New("adoption requires a physical target home")
	}
	if !os.SameFile(baseInfo, baseInfo) {
		return nil, false, errors.New("cannot capture the adoption home identity")
	}
	root, err := os.OpenRoot(p.base)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	openedRoot, err := root.Stat(".")
	if err != nil || !os.SameFile(baseInfo, openedRoot) {
		return nil, false, errors.New("adoption home changed while opening")
	}
	relative, err := filepath.Rel(p.base, path)
	if err != nil {
		return nil, false, err
	}
	info, err := root.Lstat(relative)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if redirected(info) || !info.Mode().IsRegular() || info.Size() > maxAdoptionAssetBytes {
		return nil, false, fmt.Errorf("native adoption configuration is nonregular or exceeds the byte limit: %s", path)
	}
	if !os.SameFile(info, info) {
		return nil, false, fmt.Errorf("cannot capture the native adoption configuration identity: %s", path)
	}
	file, err := root.OpenFile(relative, os.O_RDONLY|adoptionNonblockRead, 0)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, false, fmt.Errorf("native adoption configuration changed while opening: %s", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxAdoptionAssetBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > maxAdoptionAssetBytes {
		return nil, false, fmt.Errorf("native adoption configuration exceeds the byte limit: %s", path)
	}
	return data, true, nil
}

func (p *Plan) adoptedFileUnchanged(path string, before []byte, existed bool) error {
	actual, exists, err := p.readAdoptionFile(path)
	if err != nil {
		return err
	}
	if exists != existed || !bytes.Equal(actual, before) {
		return fmt.Errorf("file changed since planning: %s", path)
	}
	return nil
}

// Reject duplicate decoded keys at every level. JSON map decoding and JSON
// Patch otherwise need not agree which duplicate entry is being adopted.
func uniqueJSONObject(input []byte) (map[string]any, error) {
	input = bytes.TrimPrefix(input, []byte{0xef, 0xbb, 0xbf})
	standard, err := hujson.Standardize(append([]byte(nil), input...))
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(standard))
	value, err := decodeUniqueJSON(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("unexpected trailing JSON content")
	}
	document, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("configuration must be an object")
	}
	return document, nil
}

func decodeUniqueJSON(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("object key must be a string")
			}
			if _, duplicate := object[key]; duplicate {
				return nil, fmt.Errorf("duplicate JSON key %q", key)
			}
			value, err := decodeUniqueJSON(decoder)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return object, nil
	case json.Delim('['):
		array := []any{}
		for decoder.More() {
			value, err := decodeUniqueJSON(decoder)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return array, nil
	default:
		return token, nil
	}
}
