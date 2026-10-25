// Package files implements the root-scoped file API.
//
// Every path from a client goes through Resolve, which is the only thing
// standing between a configured root and the rest of the disk. It is written to
// fail closed: anything it cannot prove is inside the root is rejected
// (docs/04-API.md §6, docs/03-SECURITY.md T8).
package files

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Resolution errors. They are deliberately coarse: the client is told the path
// is invalid, never which rule caught it.
var (
	ErrInvalidPath  = errors.New("files: invalid path")
	ErrOutsideRoot  = errors.New("files: path escapes its root")
	ErrDenied       = errors.New("files: path is denylisted")
	ErrReservedName = errors.New("files: reserved file name")
	ErrNotWritable  = errors.New("files: root is read-only")
)

// DefaultDenylist is applied inside every root, case-insensitively, and cannot
// be removed by configuration (docs/03-SECURITY.md §4). A segment match is
// enough: if any component of the path matches, the path is refused.
var DefaultDenylist = []string{
	".ssh",
	".gnupg",
	".claude.json",
	".claude", // covers .claude\.credentials.json and the rest of it
	"appdata",
	"ntuser.dat",
	"ntuser.dat.log1",
	"ntuser.dat.log2",
	"$recycle.bin",
	"system volume information",
}

// denyGlobs are matched against each path segment with filepath.Match.
var denyGlobs = []string{
	"ntuser.dat*",
	"*.kdbx",
	"*.pem",
	"*.key",
	"*.pfx",
	"*.p12",
	"id_*",
	".env",
	".env.*",
}

// denyAbsolutePrefixes are absolute paths that may never be served, whatever
// the roots say. Burrow's own state holds the audit log and the device
// database.
var denyAbsolutePrefixes = []string{
	`c:\programdata\burrow`,
}

// browserProfileDirs are refused because they hold saved passwords and cookies.
var browserProfileDirs = []string{
	"google\\chrome\\user data",
	"microsoft\\edge\\user data",
	"mozilla\\firefox\\profiles",
	"bravesoftware\\brave-browser\\user data",
}

// reservedNames are Windows device names, which cannot be files.
var reservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// Resolver turns a (root, relative path) pair into an absolute path it has
// proved is inside the root.
type Resolver struct {
	// roots maps a lower-cased root name to its absolute path.
	roots map[string]resolvedRoot
	// extraDeny are the configured additions to DefaultDenylist.
	extraDeny []string
}

type resolvedRoot struct {
	name     string
	abs      string
	writable bool
}

// RootSpec is one configured root.
type RootSpec struct {
	Name     string
	Path     string
	Writable bool
}

// NewResolver builds a resolver over the configured roots. Each root path is
// made absolute and, where it exists, resolved through any symlink or junction
// now, so a root that is itself a link is compared in its real form.
func NewResolver(roots []RootSpec, extraDeny []string) (*Resolver, error) {
	r := &Resolver{roots: map[string]resolvedRoot{}, extraDeny: lowerAll(extraDeny)}
	for _, spec := range roots {
		if strings.TrimSpace(spec.Name) == "" {
			return nil, errors.New("files: root name is required")
		}
		abs, err := filepath.Abs(spec.Path)
		if err != nil {
			return nil, fmt.Errorf("files: root %q: %w", spec.Name, err)
		}
		if real, err := finalPath(abs); err == nil {
			abs = real
		}
		abs = filepath.Clean(abs)
		key := strings.ToLower(spec.Name)
		if _, dup := r.roots[key]; dup {
			return nil, fmt.Errorf("files: root %q is declared twice", spec.Name)
		}
		r.roots[key] = resolvedRoot{name: spec.Name, abs: abs, writable: spec.Writable}
	}
	return r, nil
}

// Roots lists the configured roots, for GET /api/files/roots.
func (r *Resolver) Roots() []RootSpec {
	out := make([]RootSpec, 0, len(r.roots))
	for _, rt := range r.roots {
		out = append(out, RootSpec{Name: rt.name, Path: rt.abs, Writable: rt.writable})
	}
	return out
}

// Resolved is a path that has passed every check.
type Resolved struct {
	// Abs is the absolute path on disk.
	Abs string
	// Root is the name of the root it belongs to.
	Root string
	// Rel is the cleaned, slash-separated path within the root.
	Rel string
	// Writable reports whether the root allows changes.
	Writable bool
}

// Resolve validates rootName and rel and returns the absolute path.
//
// The order matters. Syntax is rejected before the filesystem is touched, so a
// hostile path never reaches a Windows API; the containment check then runs
// against the real, link-resolved path.
func (r *Resolver) Resolve(rootName, rel string) (Resolved, error) {
	root, ok := r.roots[strings.ToLower(rootName)]
	if !ok {
		return Resolved{}, fmt.Errorf("%w: unknown root", ErrInvalidPath)
	}

	cleanRel, err := cleanRelative(rel)
	if err != nil {
		return Resolved{}, err
	}

	full := filepath.Clean(filepath.Join(root.abs, filepath.FromSlash(cleanRel)))

	// A Clean/Join that landed outside the root means the input escaped before
	// the filesystem was consulted. This catches the plain "..\..\" cases.
	if !under(root.abs, full) {
		return Resolved{}, ErrOutsideRoot
	}

	// The denylist applies at and below the root. The root's own ancestors are
	// not checked: they are a deliberate choice of root, and config already
	// refuses the two genuinely over-broad ones (a drive root, the profile).
	if err := r.checkDenylistRel(cleanRel); err != nil {
		return Resolved{}, err
	}
	if err := r.checkAbsoluteDenied(full); err != nil {
		return Resolved{}, err
	}

	// Resolve the longest existing prefix through links and junctions. A
	// junction inside the root pointing at C:\ would otherwise pass the string
	// comparison above.
	real, err := resolveExistingPrefix(full)
	if err != nil {
		return Resolved{}, err
	}
	if !under(root.abs, real) {
		return Resolved{}, ErrOutsideRoot
	}
	// The real path has to clear the denylist too: a link named innocently can
	// point at .ssh inside the same root.
	if err := r.checkDenylistRel(relativeTo(root.abs, real)); err != nil {
		return Resolved{}, err
	}
	if err := r.checkAbsoluteDenied(real); err != nil {
		return Resolved{}, err
	}

	return Resolved{Abs: full, Root: root.name, Rel: cleanRel, Writable: root.writable}, nil
}

// ResolveForCreate is Resolve plus the requirement that the root be writable.
// The name rules (reserved devices, trailing dots and spaces, forbidden
// characters) are enforced by Resolve for reads too, so there is nothing extra
// to check here.
func (r *Resolver) ResolveForCreate(rootName, rel string) (Resolved, error) {
	res, err := r.Resolve(rootName, rel)
	if err != nil {
		return Resolved{}, err
	}
	if !res.Writable {
		return Resolved{}, ErrNotWritable
	}
	return res, nil
}

// cleanRelative validates and normalises the client-supplied relative path.
//
// Every rule here is applied before the filesystem is touched. That ordering is
// load-bearing twice over: a ".." must be refused rather than quietly folded
// away (path.Clean on a rooted path drops a leading "..", which would silently
// rewrite the request into a different one), and a reserved device name must be
// refused before any Lstat — "<root>\NUL" exists on every Windows directory, so
// asking the filesystem about it first gives a misleading answer.
func cleanRelative(rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: NUL byte", ErrInvalidPath)
	}
	// Accept both separators from the client, then work in slashes.
	rel = strings.ReplaceAll(rel, `\`, "/")

	// A colon anywhere means either a drive letter or an NTFS alternate data
	// stream (file.txt:hidden). Neither is ever legitimate here.
	if strings.ContainsRune(rel, ':') {
		return "", fmt.Errorf("%w: colon", ErrInvalidPath)
	}
	// Reject UNC and extended-length prefixes explicitly, since Clean keeps
	// them.
	if strings.HasPrefix(rel, "//") {
		return "", fmt.Errorf("%w: UNC path", ErrInvalidPath)
	}

	out := make([]string, 0, 8)
	for _, seg := range strings.Split(rel, "/") {
		switch seg {
		case "", ".":
			continue // a leading "/" or a "./" is harmless noise
		case "..":
			// Refused outright, wherever it appears. "sub/../ok.txt" is a
			// legitimate way to write "ok.txt", but accepting it means the
			// resolver and the caller can disagree about which file was asked
			// for, and the only ".." that matters to an attacker is the one
			// that escapes.
			return "", ErrOutsideRoot
		}
		if err := checkSegmentName(seg); err != nil {
			return "", err
		}
		out = append(out, seg)
	}
	return strings.Join(out, "/"), nil
}

// checkSegmentName applies the Windows name rules to one path component.
// They hold for reads as well as writes: a name Windows would reinterpret is
// never the name the client meant.
func checkSegmentName(seg string) error {
	// Windows strips a trailing dot or space, so "evil." would reach "evil".
	if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
		return fmt.Errorf("%w: trailing dot or space", ErrInvalidPath)
	}
	// Characters Windows forbids in a name. They would either fail at the API
	// or be taken as a wildcard. ':' is already refused for the whole path.
	if strings.ContainsAny(seg, `<>"|?*`) {
		return fmt.Errorf("%w: forbidden character", ErrInvalidPath)
	}
	base := strings.ToLower(seg)
	if dot := strings.IndexByte(base, '.'); dot > 0 {
		base = base[:dot]
	}
	if reservedNames[base] {
		return fmt.Errorf("%w: %q", ErrReservedName, seg)
	}
	return nil
}

// checkDenylistRel tests each segment of a root-relative path.
func (r *Resolver) checkDenylistRel(rel string) error {
	if rel == "" {
		return nil
	}
	for _, seg := range strings.Split(rel, "/") {
		if r.segmentDenied(seg) {
			return ErrDenied
		}
	}
	return nil
}

// checkAbsoluteDenied tests the rules that hold for an absolute path wherever
// the roots happen to be: Burrow's own state directory and browser profiles.
func (r *Resolver) checkAbsoluteDenied(abs string) error {
	lower := strings.ToLower(filepath.Clean(abs))
	for _, p := range denyAbsolutePrefixes {
		if lower == p || strings.HasPrefix(lower, p+`\`) {
			return ErrDenied
		}
	}
	for _, b := range browserProfileDirs {
		if strings.Contains(lower, b) {
			return ErrDenied
		}
	}
	return nil
}

// relativeTo returns child expressed relative to root, slash-separated.
// It assumes under(root, child) already held.
func relativeTo(root, child string) string {
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return ""
	}
	return rel
}

// segmentDenied reports whether one path component is denylisted.
func (r *Resolver) segmentDenied(seg string) bool {
	if seg == "" || seg == "." {
		return false
	}
	s := strings.ToLower(seg)
	for _, d := range DefaultDenylist {
		if s == d {
			return true
		}
	}
	for _, g := range denyGlobs {
		if ok, err := filepath.Match(g, s); err == nil && ok {
			return true
		}
	}
	for _, d := range r.extraDeny {
		if s == d {
			return true
		}
		if ok, err := filepath.Match(d, s); err == nil && ok {
			return true
		}
	}
	return false
}

// resolveExistingPrefix resolves the longest existing ancestor of p through
// links, then re-appends the parts that do not exist yet.
//
// A path being created does not exist, so it cannot be resolved directly; its
// parent chain can be, and that is where a junction would sit.
func resolveExistingPrefix(p string) (string, error) {
	cur := filepath.Clean(p)
	var tail []string
	for {
		if _, err := os.Lstat(cur); err == nil {
			real, err := finalPath(cur)
			if err != nil {
				// If the real path cannot be determined, refuse rather than
				// fall back to the unverified string.
				return "", fmt.Errorf("%w: cannot verify", ErrInvalidPath)
			}
			parts := append([]string{real}, tail...)
			return filepath.Clean(filepath.Join(parts...)), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached a volume root without finding anything that exists.
			parts := append([]string{cur}, tail...)
			return filepath.Clean(filepath.Join(parts...)), nil
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
	}
}

// under reports whether child is root or sits inside it, comparing
// case-insensitively because Windows paths are.
func under(root, child string) bool {
	r := normalizeForCompare(root)
	c := normalizeForCompare(child)
	if c == r {
		return true
	}
	return strings.HasPrefix(c, r+string(filepath.Separator))
}

// normalizeForCompare strips the extended-length prefix and lower-cases, so
// `\\?\C:\x` and `C:\X` compare equal.
func normalizeForCompare(p string) string {
	p = filepath.Clean(p)
	p = strings.TrimPrefix(p, `\\?\`)
	p = strings.TrimSuffix(p, string(filepath.Separator))
	return strings.ToLower(p)
}

func lowerAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.ToLower(strings.TrimSpace(s)))
	}
	return out
}
