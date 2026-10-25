package files

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testResolver builds a resolver over a temp root with a known layout:
//
//	<root>/ok.txt
//	<root>/sub/deep.txt
//	<root>/.ssh/id_ed25519      (denylisted directory)
//	<outside>/secret.txt        (must never be reachable)
func testResolver(t testing.TB) (*Resolver, string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{
		filepath.Join(root, "sub"),
		filepath.Join(root, ".ssh"),
		outside,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "ok.txt"), "ok")
	write(t, filepath.Join(root, "sub", "deep.txt"), "deep")
	write(t, filepath.Join(root, ".ssh", "id_ed25519"), "PRIVATE")
	write(t, filepath.Join(outside, "secret.txt"), "SECRET")

	r, err := NewResolver([]RootSpec{
		{Name: "Docs", Path: root, Writable: true},
		{Name: "ReadOnly", Path: root, Writable: false},
	}, []string{"*.secretext"})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r, root, outside
}

func write(t testing.TB, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestResolveAccepts(t *testing.T) {
	r, root, _ := testResolver(t)
	cases := []struct{ in, wantRel string }{
		{"ok.txt", "ok.txt"},
		{"/ok.txt", "ok.txt"},
		{"./ok.txt", "ok.txt"},
		{"sub/deep.txt", "sub/deep.txt"},
		{`sub\deep.txt`, "sub/deep.txt"},
		{"sub/./deep.txt", "sub/deep.txt"},
		{"", ""},
		{"/", ""},
		{".", ""},
	}
	for _, tc := range cases {
		got, err := r.Resolve("Docs", tc.in)
		if err != nil {
			t.Errorf("Resolve(%q) = error %v, want it accepted", tc.in, err)
			continue
		}
		if got.Rel != tc.wantRel {
			t.Errorf("Resolve(%q).Rel = %q, want %q", tc.in, got.Rel, tc.wantRel)
		}
		if !under(root, got.Abs) {
			t.Errorf("Resolve(%q).Abs = %q, which is outside the root", tc.in, got.Abs)
		}
	}
}

func TestResolveRejects(t *testing.T) {
	r, _, _ := testResolver(t)

	cases := []struct {
		name string
		in   string
		want error
	}{
		{"parent traversal", "../outside/secret.txt", ErrOutsideRoot},
		{"deep traversal", "../../../../../../Windows/System32/config/SAM", ErrOutsideRoot},
		{"traversal after a real dir", "sub/../../outside/secret.txt", ErrOutsideRoot},
		{"backslash traversal", `..\outside\secret.txt`, ErrOutsideRoot},
		{"mixed slash traversal", `sub\../../outside/secret.txt`, ErrOutsideRoot},
		{"bare ..", "..", ErrOutsideRoot},
		{"inner .. that does not escape", "sub/../ok.txt", ErrOutsideRoot},
		{"trailing ..", "sub/..", ErrOutsideRoot},
		{"drive letter", `C:\Windows`, ErrInvalidPath},
		{"drive letter relative", "C:Windows", ErrInvalidPath},
		{"UNC path", `//server/share/x`, ErrInvalidPath},
		{"extended-length prefix", `\\?\C:\Windows`, ErrInvalidPath},
		{"alternate data stream", "ok.txt:hidden", ErrInvalidPath},
		{"ADS on a directory", "sub:stream/x", ErrInvalidPath},
		{"NUL byte", "ok\x00.txt", ErrInvalidPath},
		{"denylisted dir", ".ssh/id_ed25519", ErrDenied},
		{"denylisted dir cased", ".SSH/id_ed25519", ErrDenied},
		{"denylisted by glob", "sub/id_rsa", ErrDenied},
		{"denylisted pem", "sub/server.pem", ErrDenied},
		{"denylisted env", "sub/.env", ErrDenied},
		{"denylisted env suffix", "sub/.env.production", ErrDenied},
		{"denylisted kdbx", "sub/vault.kdbx", ErrDenied},
		{"configured extra denylist", "sub/notes.secretext", ErrDenied},
		{"appdata", "AppData/Roaming/x", ErrDenied},
		{"unknown root", "ok.txt", ErrInvalidPath},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := "Docs"
			if tc.name == "unknown root" {
				root = "NoSuchRoot"
			}
			_, err := r.Resolve(root, tc.in)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Resolve(%q) = %v, want it accepted", tc.in, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Resolve(%q) was accepted; want %v", tc.in, tc.want)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Resolve(%q) = %v, want %v", tc.in, err, tc.want)
			}
		})
	}
}

// TestResolveRejectsEncodedTraversal documents that percent-decoding happens in
// net/http before the resolver sees the path. The resolver must still refuse the
// decoded form, and must treat a literal "%2e%2e" as an ordinary name.
func TestResolveRejectsEncodedTraversal(t *testing.T) {
	r, _, _ := testResolver(t)
	if _, err := r.Resolve("Docs", ".."+"/outside/secret.txt"); !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("decoded traversal = %v, want ErrOutsideRoot", err)
	}
	// Still-encoded text is just a filename; it must not resolve to a parent.
	got, err := r.Resolve("Docs", "%2e%2e/x")
	if err != nil {
		// Rejection is also acceptable, but it must not escape.
		return
	}
	if !strings.Contains(strings.ToLower(got.Abs), "%2e%2e") {
		t.Errorf("literal %%2e%%2e resolved to %q; it must stay a name", got.Abs)
	}
}

func TestResolveForCreate(t *testing.T) {
	r, _, _ := testResolver(t)

	bad := []struct {
		name string
		in   string
		want error
	}{
		{"reserved CON", "CON", ErrReservedName},
		{"reserved con.txt", "sub/con.txt", ErrReservedName},
		{"reserved nul", "nul", ErrReservedName},
		{"reserved com1", "COM1", ErrReservedName},
		{"reserved lpt9", "sub/lpt9.log", ErrReservedName},
		{"trailing dot", "evil.", ErrInvalidPath},
		{"trailing space", "evil ", ErrInvalidPath},
		{"wildcard", "we*rd.txt", ErrInvalidPath},
		{"pipe", "a|b.txt", ErrInvalidPath},
		{"quote", `a"b.txt`, ErrInvalidPath},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.ResolveForCreate("Docs", tc.in); !errors.Is(err, tc.want) {
				t.Fatalf("ResolveForCreate(%q) = %v, want %v", tc.in, err, tc.want)
			}
		})
	}

	// A name that merely starts with a reserved word is fine.
	for _, ok := range []string{"console.log", "sub/computer.txt", "nullable.json"} {
		if _, err := r.ResolveForCreate("Docs", ok); err != nil {
			t.Errorf("ResolveForCreate(%q) = %v, want it accepted", ok, err)
		}
	}
}

func TestResolveForCreateRefusesReadOnlyRoot(t *testing.T) {
	r, _, _ := testResolver(t)
	if _, err := r.ResolveForCreate("ReadOnly", "new.txt"); !errors.Is(err, ErrNotWritable) {
		t.Fatalf("create in a read-only root = %v, want ErrNotWritable", err)
	}
	// Reading from it is still fine.
	if _, err := r.Resolve("ReadOnly", "ok.txt"); err != nil {
		t.Fatalf("read from a read-only root = %v, want it accepted", err)
	}
}

// TestResolveRejectsJunctionEscape creates a real directory junction inside the
// root that points outside it. A string-only containment check passes this; only
// resolving the real path catches it.
func TestResolveRejectsJunctionEscape(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("directory junctions are a Windows feature")
	}
	r, root, outside := testResolver(t)
	link := filepath.Join(root, "escape")

	// mklink /J needs no privilege, unlike a symlink.
	cmd := exec.Command("cmd", "/c", "mklink", "/J", link, outside)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not create a junction (%v): %s", err, out)
	}

	if _, err := r.Resolve("Docs", "escape/secret.txt"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("junction escape = %v, want ErrOutsideRoot", err)
	}
	if _, err := r.Resolve("Docs", "escape"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("junction itself = %v, want ErrOutsideRoot", err)
	}
}

// TestResolveRejectsSymlinkEscape is the same check for a file symlink, which
// needs Developer Mode or admin on Windows and is skipped when unavailable.
func TestResolveRejectsSymlinkEscape(t *testing.T) {
	r, root, outside := testResolver(t)
	link := filepath.Join(root, "slink.txt")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if _, err := r.Resolve("Docs", "slink.txt"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("symlink escape = %v, want ErrOutsideRoot", err)
	}
}

// TestResolveAllowsInternalLink confirms the check is not simply "reject all
// links": a link that stays inside the root is legitimate.
func TestResolveAllowsInternalLink(t *testing.T) {
	r, root, _ := testResolver(t)
	link := filepath.Join(root, "inside.txt")
	if err := os.Symlink(filepath.Join(root, "ok.txt"), link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if _, err := r.Resolve("Docs", "inside.txt"); err != nil {
		t.Fatalf("link inside the root = %v, want it accepted", err)
	}
}

func TestNewResolverRejectsDuplicateRootNames(t *testing.T) {
	dir := t.TempDir()
	_, err := NewResolver([]RootSpec{
		{Name: "Docs", Path: dir},
		{Name: "docs", Path: dir},
	}, nil)
	if err == nil {
		t.Fatal("NewResolver accepted two roots with the same name")
	}
}

// FuzzResolve asserts the one invariant that matters: whatever the input, a
// successful resolution is inside the root and clear of the denylist.
func FuzzResolve(f *testing.F) {
	r, root, _ := testResolver(f)

	seeds := []string{
		"ok.txt", "sub/deep.txt", "../x", "..\\x", "..", "../../..",
		`C:\Windows`, `\\?\C:\x`, "//srv/share", "a:b", "a\x00b",
		".ssh/id_ed25519", "AppData/x", "%2e%2e/x", "CON", "evil.", "evil ",
		"sub/../ok.txt", "sub/////deep.txt", "./././ok.txt",
		strings.Repeat("a/", 200) + "x", strings.Repeat("../", 200) + "x",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, rel string) {
		res, err := r.Resolve("Docs", rel)
		if err != nil {
			return // a refusal is always a safe answer
		}
		if !under(root, res.Abs) {
			t.Fatalf("Resolve(%q) accepted %q, which is outside root %q", rel, res.Abs, root)
		}
		// An accepted path must also be clear of the denylist, checked
		// independently of the code path that produced it. Only the part at or
		// below the root is in scope — the root's own ancestors are a
		// configuration choice, not client input (and under `go test` the root
		// itself lives in a temp dir below AppData).
		for _, seg := range strings.Split(strings.ToLower(res.Rel), "/") {
			switch seg {
			case ".ssh", ".gnupg", ".claude", ".claude.json", "appdata":
				t.Fatalf("Resolve(%q) accepted a denylisted segment %q (rel %q)", rel, seg, res.Rel)
			}
		}
		// The relative form must never contain a climbing segment.
		for _, seg := range strings.Split(res.Rel, "/") {
			if seg == ".." {
				t.Fatalf("Resolve(%q).Rel = %q still contains ..", rel, res.Rel)
			}
		}
	})
}
