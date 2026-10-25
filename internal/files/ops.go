package files

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Operation errors.
var (
	ErrExists    = errors.New("files: already exists")
	ErrTooLarge  = errors.New("files: larger than the upload limit")
	ErrIsDir     = errors.New("files: is a directory")
	ErrNotExist  = errors.New("files: does not exist")
	ErrCrossRoot = errors.New("files: cannot move between roots")
)

// Entry is one row in a directory listing (docs/04-API.md §6).
type Entry struct {
	Name        string    `json:"name"`
	Type        string    `json:"type"` // "file" | "dir"
	Size        int64     `json:"size"`
	MTime       time.Time `json:"mtime"`
	Previewable bool      `json:"previewable"`
}

// Service performs file operations inside the resolver's roots.
type Service struct {
	r           *Resolver
	maxUploadMB int64
	// recycle moves a path to the Recycle Bin. Replaced in tests, and a plain
	// delete on non-Windows.
	recycle func(path string) error
}

// NewService builds the file service.
func NewService(r *Resolver, maxUploadMB int64) *Service {
	if maxUploadMB <= 0 {
		maxUploadMB = 2048
	}
	return &Service{r: r, maxUploadMB: maxUploadMB, recycle: recycleBin}
}

// Resolver exposes the underlying resolver, for the roots endpoint.
func (s *Service) Resolver() *Resolver { return s.r }

// MaxUploadBytes is the configured upload cap.
func (s *Service) MaxUploadBytes() int64 { return s.maxUploadMB << 20 }

// List returns a directory's entries, with denylisted names omitted entirely
// rather than shown-and-refused: a listing should not advertise what it will
// not serve.
func (s *Service) List(root, path, sortBy string) ([]Entry, error) {
	res, err := s.r.Resolve(root, path)
	if err != nil {
		return nil, err
	}
	dirEntries, err := os.ReadDir(res.Abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotExist
		}
		return nil, fmt.Errorf("files: list %s: %w", res.Rel, err)
	}

	out := make([]Entry, 0, len(dirEntries))
	for _, de := range dirEntries {
		if s.r.segmentDenied(de.Name()) {
			continue
		}
		// A name that resolves outside the root (a junction) is hidden too.
		child := joinRel(res.Rel, de.Name())
		if _, err := s.r.Resolve(root, child); err != nil {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue // vanished between ReadDir and Info
		}
		e := Entry{
			Name:  de.Name(),
			Type:  "file",
			Size:  info.Size(),
			MTime: info.ModTime().UTC(),
		}
		if de.IsDir() {
			e.Type = "dir"
			e.Size = 0
		} else {
			e.Previewable = InlineAllowedFor(de.Name())
		}
		out = append(out, e)
	}

	sortEntries(out, sortBy)
	return out, nil
}

// sortEntries orders a listing. Directories come first in every mode, which is
// what a file browser is expected to do.
func sortEntries(out []Entry, sortBy string) {
	less := func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	}
	switch sortBy {
	case "mtime":
		less = func(i, j int) bool { return out[i].MTime.After(out[j].MTime) }
	case "size":
		less = func(i, j int) bool { return out[i].Size > out[j].Size }
	}
	sort.SliceStable(out, func(i, j int) bool {
		di, dj := out[i].Type == "dir", out[j].Type == "dir"
		if di != dj {
			return di
		}
		return less(i, j)
	})
}

// Stat returns information about one path.
func (s *Service) Stat(root, path string) (Resolved, os.FileInfo, error) {
	res, err := s.r.Resolve(root, path)
	if err != nil {
		return Resolved{}, nil, err
	}
	info, err := os.Stat(res.Abs)
	if errors.Is(err, os.ErrNotExist) {
		return Resolved{}, nil, ErrNotExist
	}
	if err != nil {
		return Resolved{}, nil, fmt.Errorf("files: stat: %w", err)
	}
	return res, info, nil
}

// Open opens a file for reading. The caller closes it.
func (s *Service) Open(root, path string) (*os.File, os.FileInfo, error) {
	res, info, err := s.Stat(root, path)
	if err != nil {
		return nil, nil, err
	}
	if info.IsDir() {
		return nil, nil, ErrIsDir
	}
	f, err := os.Open(res.Abs)
	if err != nil {
		return nil, nil, fmt.Errorf("files: open: %w", err)
	}
	return f, info, nil
}

// Mkdir creates a directory, including missing parents.
func (s *Service) Mkdir(root, path string) error {
	res, err := s.r.ResolveForCreate(root, path)
	if err != nil {
		return err
	}
	if res.Rel == "" {
		return fmt.Errorf("%w: the root already exists", ErrExists)
	}
	if _, err := os.Stat(res.Abs); err == nil {
		return ErrExists
	}
	if err := os.MkdirAll(res.Abs, 0o755); err != nil {
		return fmt.Errorf("files: mkdir: %w", err)
	}
	return nil
}

// Rename moves a file within one root.
func (s *Service) Rename(root, from, to string) error {
	src, err := s.r.ResolveForCreate(root, from)
	if err != nil {
		return err
	}
	dst, err := s.r.ResolveForCreate(root, to)
	if err != nil {
		return err
	}
	if src.Rel == "" || dst.Rel == "" {
		return fmt.Errorf("%w: cannot rename a root", ErrInvalidPath)
	}
	if _, err := os.Stat(src.Abs); errors.Is(err, os.ErrNotExist) {
		return ErrNotExist
	}
	if _, err := os.Stat(dst.Abs); err == nil {
		return ErrExists
	}
	if err := os.Rename(src.Abs, dst.Abs); err != nil {
		return fmt.Errorf("files: rename: %w", err)
	}
	return nil
}

// Delete moves a path to the Recycle Bin rather than unlinking it, so a mistap
// on a phone is recoverable (docs/04-API.md §6).
func (s *Service) Delete(root, path string) error {
	res, err := s.r.ResolveForCreate(root, path)
	if err != nil {
		return err
	}
	if res.Rel == "" {
		return fmt.Errorf("%w: cannot delete a root", ErrInvalidPath)
	}
	if _, err := os.Stat(res.Abs); errors.Is(err, os.ErrNotExist) {
		return ErrNotExist
	}
	if err := s.recycle(res.Abs); err != nil {
		return err
	}
	return nil
}

// Upload streams a file into a root.
//
// It writes to a temp file in the destination directory and renames it into
// place, so a half-finished upload is never visible under the real name and a
// failure leaves nothing behind.
func (s *Service) Upload(root, path string, body io.Reader, overwrite bool) (int64, error) {
	res, err := s.r.ResolveForCreate(root, path)
	if err != nil {
		return 0, err
	}
	if res.Rel == "" {
		return 0, fmt.Errorf("%w: no filename", ErrInvalidPath)
	}
	if !overwrite {
		if _, err := os.Stat(res.Abs); err == nil {
			return 0, ErrExists
		}
	}
	dir := filepath.Dir(res.Abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("files: create folder: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".burrow-upload-*")
	if err != nil {
		return 0, fmt.Errorf("files: temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	limit := s.MaxUploadBytes()
	// One byte over the limit is read deliberately, so hitting exactly the cap
	// succeeds and anything larger is refused rather than silently truncated.
	n, err := io.Copy(tmp, io.LimitReader(body, limit+1))
	if err != nil {
		cleanup()
		return 0, fmt.Errorf("files: upload: %w", err)
	}
	if n > limit {
		cleanup()
		return 0, fmt.Errorf("%w (%d MB)", ErrTooLarge, s.maxUploadMB)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return 0, fmt.Errorf("files: flush upload: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return 0, fmt.Errorf("files: close upload: %w", err)
	}

	// Windows Rename fails onto an existing file, so an overwrite replaces the
	// target explicitly.
	if overwrite {
		if err := os.Remove(res.Abs); err != nil && !errors.Is(err, os.ErrNotExist) {
			os.Remove(tmpName)
			return 0, fmt.Errorf("files: replace existing file: %w", err)
		}
	}
	if err := os.Rename(tmpName, res.Abs); err != nil {
		os.Remove(tmpName)
		return 0, fmt.Errorf("files: finish upload: %w", err)
	}
	return n, nil
}

// Search matches file names under a root, bounded in both results and time so
// a deep tree cannot tie up the agent (docs/04-API.md §6).
func (s *Service) Search(root, query string, max int, budget time.Duration) ([]string, error) {
	if max <= 0 || max > 200 {
		max = 200
	}
	if budget <= 0 {
		budget = 5 * time.Second
	}
	res, err := s.r.Resolve(root, "")
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, fmt.Errorf("%w: empty query", ErrInvalidPath)
	}

	deadline := time.Now().Add(budget)
	var out []string
	err = filepath.WalkDir(res.Abs, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // an unreadable directory is skipped, not fatal
		}
		if time.Now().After(deadline) || len(out) >= max {
			return filepath.SkipAll
		}
		if s.r.segmentDenied(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.Contains(strings.ToLower(d.Name()), q) {
			return nil
		}
		rel, relErr := filepath.Rel(res.Abs, p)
		if relErr != nil {
			return nil
		}
		// Confirm through the resolver, so a junction encountered mid-walk
		// cannot put an outside path in the results.
		if _, err := s.r.Resolve(root, filepath.ToSlash(rel)); err != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("files: search: %w", err)
	}
	return out, nil
}

// ---------- content types ----------

// previewExtensions are the types safe to render in the browser. HTML and SVG
// are absent: both can carry script, and serving one inline from the agent's
// origin would give an uploaded file the app's cookies.
var previewExtensions = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".png": "image/png", ".gif": "image/gif", ".webp": "image/webp",
	".pdf": "application/pdf",
	".txt": "text/plain; charset=utf-8", ".md": "text/plain; charset=utf-8",
	".log": "text/plain; charset=utf-8", ".json": "text/plain; charset=utf-8",
	".yaml": "text/plain; charset=utf-8", ".yml": "text/plain; charset=utf-8",
	".csv": "text/plain; charset=utf-8", ".ini": "text/plain; charset=utf-8",
	".go": "text/plain; charset=utf-8", ".ts": "text/plain; charset=utf-8",
	".js": "text/plain; charset=utf-8", ".py": "text/plain; charset=utf-8",
	".sh": "text/plain; charset=utf-8", ".ps1": "text/plain; charset=utf-8",
	".sql": "text/plain; charset=utf-8", ".toml": "text/plain; charset=utf-8",
	".css": "text/plain; charset=utf-8", ".xml": "text/plain; charset=utf-8",
}

// PreviewTypeFor returns the content type to serve a file inline as, or "".
//
// Source files are offered as text/plain, never as their own type: a .js served
// as JavaScript or a .css as a stylesheet would be active content.
func PreviewTypeFor(name string) string {
	return previewExtensions[strings.ToLower(filepath.Ext(name))]
}

// InlineAllowedFor reports whether a file may be previewed.
func InlineAllowedFor(name string) bool { return PreviewTypeFor(name) != "" }

// DownloadTypeFor returns the content type for a download. Anything not on the
// preview list is served as a generic binary with an attachment disposition.
func DownloadTypeFor(name string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); t != "" && InlineAllowedFor(name) {
		return t
	}
	return "application/octet-stream"
}

// MaxPreviewTextBytes caps inline text previews (docs/04-API.md §6).
const MaxPreviewTextBytes = 1 << 20

func joinRel(base, name string) string {
	if base == "" {
		return name
	}
	return base + "/" + name
}
