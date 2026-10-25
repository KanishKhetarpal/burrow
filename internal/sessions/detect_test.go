package sessions

import (
	"strings"
	"testing"
)

// Fixtures mirror what Claude Code actually writes to a PTY, escape codes
// included, so the detectors are exercised against coloured output rather than
// clean text.
const (
	fixtureTrust = "\x1b[?25l\x1b[2J\x1b[H\x1b[1m\x1b[38;5;214m Claude Code\x1b[0m\r\n" +
		"\r\n\x1b[1mDo you trust the files in this folder?\x1b[0m\r\n" +
		"\x1b[2mD:\\code\\demo-app\x1b[0m\r\n\r\n" +
		"Trust the files in this folder? \x1b[2m[y/N]\x1b[0m "

	fixtureEnableRC = "\x1b[32m✓\x1b[0m Starting session\r\n" +
		"Enable Remote Control? \x1b[2m(y/n)\x1b[0m "

	fixtureRemoteURL = "\x1b[32m✓\x1b[0m Remote Control is on\r\n" +
		"  Open this session at \x1b[4mhttps://claude.ai/code/s/AbC123_def-456\x1b[0m\r\n" +
		"\r\n\x1b[2mWaiting for a message…\x1b[0m\r\n"

	fixturePermission = "\x1b[1mBash\x1b[0m\x1b[2m(rm -rf build)\x1b[0m\r\n" +
		"\r\nDo you want to run this command?\r\n" +
		"\x1b[36m 1.\x1b[0m Yes\r\n" +
		"\x1b[36m 2.\x1b[0m Yes, and don't ask again for Bash commands\r\n" +
		"\x1b[36m 3.\x1b[0m No, and tell Claude what to do differently\r\n" +
		"\r\n\x1b[2m↑/↓ to choose, Enter to confirm\x1b[0m"

	fixtureIdle = "\x1b[2m╭──────────────────────────╮\x1b[0m\r\n" +
		"\x1b[2m│ > \x1b[0m                      \x1b[2m│\x1b[0m\r\n" +
		"\x1b[2m╰──────────────────────────╯\x1b[0m\r\n" +
		"\x1b[2m? for shortcuts\x1b[0m\r\n"
)

func TestStripANSI(t *testing.T) {
	got := StripANSI(fixtureTrust)
	for _, forbidden := range []string{"\x1b", "[1m", "[38;5;214m", "\r"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("stripped text still contains %q:\n%q", forbidden, got)
		}
	}
	if !strings.Contains(got, "Do you trust the files in this folder?") {
		t.Errorf("stripping removed the text as well:\n%q", got)
	}
	// An OSC title sequence must go too, including its terminator.
	osc := "\x1b]0;C:\\WINDOWS\\system32\\cmd.exe\x07hello"
	if got := StripANSI(osc); got != "hello" {
		t.Errorf("StripANSI(osc) = %q, want %q", got, "hello")
	}
}

func TestFindRemoteURL(t *testing.T) {
	got := FindRemoteURL(fixtureRemoteURL)
	want := "https://claude.ai/code/s/AbC123_def-456"
	if got != want {
		t.Errorf("FindRemoteURL = %q, want %q", got, want)
	}

	// No URL in ordinary output.
	if got := FindRemoteURL(fixtureIdle); got != "" {
		t.Errorf("FindRemoteURL on idle output = %q, want empty", got)
	}
	// A trailing full stop is not part of the URL.
	if got := FindRemoteURL("see https://claude.ai/code/abc123."); got != "https://claude.ai/code/abc123" {
		t.Errorf("FindRemoteURL kept trailing punctuation: %q", got)
	}
	// Query strings survive.
	in := "https://claude.ai/code/s/x1?from=cli&v=2"
	if got := FindRemoteURL(in); got != in {
		t.Errorf("FindRemoteURL = %q, want the whole URL %q", got, in)
	}
	// A look-alike host must not match.
	if got := FindRemoteURL("https://claude.ai.evil.example/code/abc"); got != "" {
		t.Errorf("FindRemoteURL matched a look-alike host: %q", got)
	}
}

func TestDetectPrompt(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantKind PromptKind
		wantOpts []string
		wantText string // substring
	}{
		{"trust folder", fixtureTrust, PromptTrust, []string{"y", "n"}, "Trust the files"},
		{"enable remote control", fixtureEnableRC, PromptEnableRC, []string{"y", "n"}, "Enable Remote Control"},
		{"permission with numbered options", fixturePermission, PromptPermission,
			[]string{"1", "2", "3"}, "Do you want to run this command"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := DetectPrompt(tc.in)
			if !ok {
				t.Fatalf("DetectPrompt found nothing")
			}
			if p.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", p.Kind, tc.wantKind)
			}
			if !strings.Contains(p.Text, tc.wantText) {
				t.Errorf("Text = %q, want it to contain %q", p.Text, tc.wantText)
			}
			if strings.Join(p.Options, ",") != strings.Join(tc.wantOpts, ",") {
				t.Errorf("Options = %v, want %v", p.Options, tc.wantOpts)
			}
			// The surfaced text must be clean enough to put on a button.
			if strings.Contains(p.Text, "\x1b") {
				t.Errorf("Text still carries escape codes: %q", p.Text)
			}
		})
	}
}

func TestDetectPromptIgnoresOrdinaryOutput(t *testing.T) {
	for _, in := range []string{
		fixtureIdle,
		"",
		"   \r\n\r\n",
		fixtureRemoteURL,
		"Running tests...\r\nok  burrow/internal/files  0.8s\r\n",
		// Prose that mentions trust but asks nothing.
		"I trust this will work.\r\n",
		// A single numbered line is prose, not a dialog.
		"Here is the plan:\r\n 1. Read the spec\r\n",
	} {
		if p, ok := DetectPrompt(in); ok {
			t.Errorf("DetectPrompt(%.40q) found %+v, want nothing", in, p)
		}
	}
}

func TestDetectPromptPrefersTheSpecificKind(t *testing.T) {
	// Output where a trust prompt follows a permission-shaped line: the
	// specific prompt is the one the user is actually looking at.
	in := fixturePermission + "\r\n" + fixtureTrust
	p, ok := DetectPrompt(in)
	if !ok {
		t.Fatal("DetectPrompt found nothing")
	}
	if p.Kind != PromptTrust {
		t.Errorf("Kind = %q, want %q (the specific prompt wins)", p.Kind, PromptTrust)
	}
}

func TestDetectPromptFallsBackToYesNo(t *testing.T) {
	p, ok := DetectPrompt("Overwrite the existing file? [y/N] ")
	if !ok {
		t.Fatal("DetectPrompt found nothing on a bare yes/no question")
	}
	if p.Kind != PromptOther {
		t.Errorf("Kind = %q, want %q", p.Kind, PromptOther)
	}
	if strings.Join(p.Options, ",") != "y,n" {
		t.Errorf("Options = %v, want [y n]", p.Options)
	}
}

func TestKeyBytes(t *testing.T) {
	cases := map[string][]byte{
		"enter":     []byte("\r"),
		"esc":       {0x1b},
		"escape":    {0x1b},
		"ctrl_c":    {0x03},
		"tab":       []byte("\t"),
		"up":        []byte("\x1b[A"),
		"down":      []byte("\x1b[B"),
		"left":      []byte("\x1b[D"),
		"right":     []byte("\x1b[C"),
		"backspace": {0x7f},
		"  Enter ":  []byte("\r"),
		"CTRL_C":    {0x03},
	}
	for name, want := range cases {
		got, ok := KeyBytes(name)
		if !ok {
			t.Errorf("KeyBytes(%q) was not recognised", name)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("KeyBytes(%q) = %q, want %q", name, got, want)
		}
	}
	for _, bad := range []string{"", "f13", "ctrl_alt_del", "\x03"} {
		if _, ok := KeyBytes(bad); ok {
			t.Errorf("KeyBytes(%q) was accepted, want it refused", bad)
		}
	}
}
