package files

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// opsFixture builds a service over a writable and a read-only root, with a
// recycle function that records what it was asked to remove instead of touching
// the real Recycle Bin.
type opsFixture struct {
	s        *Service
	root     string
	recycled []string
}

func newOpsFixture(t *testing.T) *opsFixture {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "notes.txt"), "hello")
	write(t, filepath.Join(root, "photo.png"), "\x89PNG\r\n")
	write(t, filepath.Join(root, "secret.pem"), "PRIVATE")
	write(t, filepath.Join(root, "sub", "deep.md"), "# deep")
	write(t, filepath.Join(root, ".ssh", "id_ed25519"), "KEY")

	r, err := NewResolver([]RootSpec{
		{Name: "Docs", Path: root, Writable: true},
		{Name: "ReadOnly", Path: root, Writable: false},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &opsFixture{root: root}
	f.s = NewService(r, 1) // 1 MB cap, so the limit is cheap to test
	f.s.recycle = func(p string) error {
		f.recycled = append(f.recycled, p)
		return os.RemoveAll(p)
	}
	return f
}

func names(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

func TestListHidesDenylistedEntries(t *testing.T) {
	f := newOpsFixture(t)
	entries, err := f.s.List("Docs", "", "name")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := strings.Join(names(entries), ",")
	// Directories first, then files by name.
	if got != "sub,notes.txt,photo.png" {
		t.Errorf("List = %q, want \"sub,notes.txt,photo.png\"", got)
	}
	for _, hidden := range []string{".ssh", "secret.pem"} {
		if strings.Contains(got, hidden) {
			t.Errorf("List exposed the denylisted entry %q", hidden)
		}
	}
}

func TestListMarksPreviewableFiles(t *testing.T) {
	f := newOpsFixture(t)
	entries, err := f.s.List("Docs", "", "name")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		switch e.Name {
		case "notes.txt", "photo.png":
			if !e.Previewable {
				t.Errorf("%s is not marked previewable", e.Name)
			}
		case "sub":
			if e.Previewable {
				t.Error("a directory is marked previewable")
			}
		}
	}
}

func TestListSortModes(t *testing.T) {
	f := newOpsFixture(t)
	// Make notes.txt clearly the newest.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(f.root, "notes.txt"), later, later); err != nil {
		t.Fatal(err)
	}
	entries, err := f.s.List("Docs", "", "mtime")
	if err != nil {
		t.Fatal(err)
	}
	// The directory still leads; among files, newest first.
	files := names(entries)[1:]
	if files[0] != "notes.txt" {
		t.Errorf("mtime sort = %v, want notes.txt first among files", files)
	}
}

func TestListRefusesDenylistedAndUnknownPaths(t *testing.T) {
	f := newOpsFixture(t)
	if _, err := f.s.List("Docs", ".ssh", "name"); !errors.Is(err, ErrDenied) {
		t.Errorf("List(.ssh) = %v, want ErrDenied", err)
	}
	if _, err := f.s.List("Docs", "nope", "name"); !errors.Is(err, ErrNotExist) {
		t.Errorf("List(nope) = %v, want ErrNotExist", err)
	}
	if _, err := f.s.List("Nope", "", "name"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("List on an unknown root = %v, want ErrInvalidPath", err)
	}
}

func TestOpenAndStat(t *testing.T) {
	f := newOpsFixture(t)
	file, info, err := f.s.Open("Docs", "notes.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer file.Close()
	if info.Size() != 5 {
		t.Errorf("Size = %d, want 5", info.Size())
	}
	body, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello" {
		t.Errorf("content = %q, want \"hello\"", body)
	}

	if _, _, err := f.s.Open("Docs", "sub"); !errors.Is(err, ErrIsDir) {
		t.Errorf("Open on a directory = %v, want ErrIsDir", err)
	}
	if _, _, err := f.s.Open("Docs", "secret.pem"); !errors.Is(err, ErrDenied) {
		t.Errorf("Open on a denylisted file = %v, want ErrDenied", err)
	}
}

func TestMkdir(t *testing.T) {
	f := newOpsFixture(t)
	if err := f.s.Mkdir("Docs", "new/nested"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if st, err := os.Stat(filepath.Join(f.root, "new", "nested")); err != nil || !st.IsDir() {
		t.Fatalf("the directory was not created: %v", err)
	}
	if err := f.s.Mkdir("Docs", "new/nested"); !errors.Is(err, ErrExists) {
		t.Errorf("Mkdir on an existing path = %v, want ErrExists", err)
	}
	if err := f.s.Mkdir("ReadOnly", "nope"); !errors.Is(err, ErrNotWritable) {
		t.Errorf("Mkdir in a read-only root = %v, want ErrNotWritable", err)
	}
	if err := f.s.Mkdir("Docs", "CON"); !errors.Is(err, ErrReservedName) {
		t.Errorf("Mkdir(CON) = %v, want ErrReservedName", err)
	}
}

func TestRename(t *testing.T) {
	f := newOpsFixture(t)
	if err := f.s.Rename("Docs", "notes.txt", "sub/renamed.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "sub", "renamed.txt")); err != nil {
		t.Fatalf("the file was not moved: %v", err)
	}

	if err := f.s.Rename("Docs", "missing.txt", "x.txt"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Rename of a missing file = %v, want ErrNotExist", err)
	}
	// Renaming onto an existing name must not clobber it.
	write(t, filepath.Join(f.root, "a.txt"), "a")
	write(t, filepath.Join(f.root, "b.txt"), "b")
	if err := f.s.Rename("Docs", "a.txt", "b.txt"); !errors.Is(err, ErrExists) {
		t.Errorf("Rename onto an existing file = %v, want ErrExists", err)
	}
	if body, _ := os.ReadFile(filepath.Join(f.root, "b.txt")); string(body) != "b" {
		t.Error("the existing file was overwritten by a rename")
	}
	// Escapes are refused.
	if err := f.s.Rename("Docs", "b.txt", "../escaped.txt"); !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("Rename out of the root = %v, want ErrOutsideRoot", err)
	}
}

func TestDeleteGoesToTheRecycleBin(t *testing.T) {
	f := newOpsFixture(t)
	if err := f.s.Delete("Docs", "notes.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(f.recycled) != 1 {
		t.Fatalf("recycled %d paths, want 1", len(f.recycled))
	}
	if !strings.HasSuffix(f.recycled[0], "notes.txt") {
		t.Errorf("recycled %q, want notes.txt", f.recycled[0])
	}
	if _, err := os.Stat(filepath.Join(f.root, "notes.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the file is still in place after Delete")
	}

	if err := f.s.Delete("Docs", "missing"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Delete of a missing path = %v, want ErrNotExist", err)
	}
	if err := f.s.Delete("ReadOnly", "photo.png"); !errors.Is(err, ErrNotWritable) {
		t.Errorf("Delete in a read-only root = %v, want ErrNotWritable", err)
	}
	// The root itself is not deletable.
	if err := f.s.Delete("Docs", ""); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("Delete of the root = %v, want ErrInvalidPath", err)
	}
}

func TestUpload(t *testing.T) {
	f := newOpsFixture(t)
	n, err := f.s.Upload("Docs", "uploads/new.txt", strings.NewReader("uploaded"), false)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if n != 8 {
		t.Errorf("wrote %d bytes, want 8", n)
	}
	body, err := os.ReadFile(filepath.Join(f.root, "uploads", "new.txt"))
	if err != nil {
		t.Fatalf("the upload is not on disk: %v", err)
	}
	if string(body) != "uploaded" {
		t.Errorf("content = %q, want \"uploaded\"", body)
	}
}

func TestUploadDoesNotOverwriteByDefault(t *testing.T) {
	f := newOpsFixture(t)
	if _, err := f.s.Upload("Docs", "notes.txt", strings.NewReader("replacement"), false); !errors.Is(err, ErrExists) {
		t.Fatalf("Upload onto an existing file = %v, want ErrExists", err)
	}
	if body, _ := os.ReadFile(filepath.Join(f.root, "notes.txt")); string(body) != "hello" {
		t.Errorf("the original was modified: %q", body)
	}

	// With overwrite requested it replaces it.
	if _, err := f.s.Upload("Docs", "notes.txt", strings.NewReader("replacement"), true); err != nil {
		t.Fatalf("Upload with overwrite: %v", err)
	}
	if body, _ := os.ReadFile(filepath.Join(f.root, "notes.txt")); string(body) != "replacement" {
		t.Errorf("content = %q, want \"replacement\"", body)
	}
}

func TestUploadEnforcesTheSizeCap(t *testing.T) {
	f := newOpsFixture(t)
	limit := f.s.MaxUploadBytes()

	// Exactly at the cap is allowed.
	if _, err := f.s.Upload("Docs", "atlimit.bin", strings.NewReader(strings.Repeat("x", int(limit))), false); err != nil {
		t.Fatalf("Upload at exactly the cap: %v", err)
	}
	// One byte over is refused.
	_, err := f.s.Upload("Docs", "over.bin", strings.NewReader(strings.Repeat("x", int(limit)+1)), false)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Upload over the cap = %v, want ErrTooLarge", err)
	}
	// And nothing is left behind.
	if _, err := os.Stat(filepath.Join(f.root, "over.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a refused upload left a partial file in place")
	}
	leftovers, _ := filepath.Glob(filepath.Join(f.root, ".burrow-upload-*"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

func TestUploadRefusesDenylistedAndEscapingPaths(t *testing.T) {
	f := newOpsFixture(t)
	cases := map[string]error{
		"../escaped.txt":    ErrOutsideRoot,
		".ssh/authorized":   ErrDenied,
		"creds.pem":         ErrDenied,
		"CON":               ErrReservedName,
		"trailing.":         ErrInvalidPath,
		"stream.txt:hidden": ErrInvalidPath,
	}
	for path, want := range cases {
		_, err := f.s.Upload("Docs", path, strings.NewReader("x"), false)
		if !errors.Is(err, want) {
			t.Errorf("Upload(%q) = %v, want %v", path, err, want)
		}
	}
}

func TestUploadRefusedInReadOnlyRoot(t *testing.T) {
	f := newOpsFixture(t)
	if _, err := f.s.Upload("ReadOnly", "x.txt", strings.NewReader("x"), false); !errors.Is(err, ErrNotWritable) {
		t.Fatalf("Upload into a read-only root = %v, want ErrNotWritable", err)
	}
}

func TestSearch(t *testing.T) {
	f := newOpsFixture(t)
	got, err := f.s.Search("Docs", "deep", 50, 5*time.Second)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0] != "sub/deep.md" {
		t.Errorf("Search = %v, want [sub/deep.md]", got)
	}

	// Denylisted trees are not searched.
	hits, err := f.s.Search("Docs", "id_ed25519", 50, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("Search found denylisted files: %v", hits)
	}
	if _, err := f.s.Search("Docs", "  ", 50, time.Second); err == nil {
		t.Error("Search accepted an empty query")
	}
}

func TestSearchRespectsTheResultCap(t *testing.T) {
	f := newOpsFixture(t)
	for i := 0; i < 20; i++ {
		write(t, filepath.Join(f.root, "match"+string(rune('a'+i))+".txt"), "x")
	}
	got, err := f.s.Search("Docs", "match", 5, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 5 {
		t.Errorf("Search returned %d results, want at most 5", len(got))
	}
}

func TestPreviewTypes(t *testing.T) {
	cases := map[string]string{
		"a.png":  "image/png",
		"a.jpg":  "image/jpeg",
		"a.pdf":  "application/pdf",
		"a.txt":  "text/plain; charset=utf-8",
		"a.md":   "text/plain; charset=utf-8",
		"a.json": "text/plain; charset=utf-8",
		// Source files are offered as plain text, never as active content.
		"a.js":  "text/plain; charset=utf-8",
		"a.css": "text/plain; charset=utf-8",
	}
	for name, want := range cases {
		if got := PreviewTypeFor(name); got != want {
			t.Errorf("PreviewTypeFor(%q) = %q, want %q", name, got, want)
		}
	}
	// HTML and SVG must never be previewable.
	for _, name := range []string{"a.html", "a.htm", "a.svg", "a.xhtml", "a.exe", "a.zip"} {
		if got := PreviewTypeFor(name); got != "" {
			t.Errorf("PreviewTypeFor(%q) = %q, want it refused", name, got)
		}
		if InlineAllowedFor(name) {
			t.Errorf("InlineAllowedFor(%q) = true, want false", name)
		}
	}
}

func TestDownloadTypeFallsBackToOctetStream(t *testing.T) {
	for _, name := range []string{"a.html", "a.svg", "a.exe", "a.unknownext"} {
		if got := DownloadTypeFor(name); got != "application/octet-stream" {
			t.Errorf("DownloadTypeFor(%q) = %q, want application/octet-stream", name, got)
		}
	}
}
