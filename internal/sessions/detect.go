package sessions

import (
	"regexp"
	"strings"
)

// Detection is best-effort by design. The raw terminal is always available in
// the PWA, so a missed prompt costs the user a tap, not the session
// (docs/04-API.md §5.4).

// PromptKind classifies a question Claude Code is waiting on.
type PromptKind string

const (
	PromptTrust      PromptKind = "trust"
	PromptEnableRC   PromptKind = "enable_rc"
	PromptPermission PromptKind = "permission"
	PromptOther      PromptKind = "other"
)

// Prompt is a detected question, surfaced as quick-reply buttons.
type Prompt struct {
	Kind    PromptKind `json:"kind"`
	Text    string     `json:"text"`
	Options []string   `json:"options,omitempty"`
}

// ansiPattern matches CSI, OSC and the single-character escapes that Claude
// Code's TUI emits. Stripping them first means the text patterns below do not
// have to tolerate colour codes in the middle of a word.
var ansiPattern = regexp.MustCompile(
	// OSC ... BEL or ST
	`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)` +
		// CSI ... final byte
		`|\x1b\[[0-?]*[ -/]*[@-~]` +
		// two-character escapes and charset selection
		`|\x1b[()#][0-9A-Za-z]` +
		`|\x1b[=><NOM78cDEHZ]`)

// StripANSI removes escape sequences and carriage returns from PTY output.
func StripANSI(s string) string {
	s = ansiPattern.ReplaceAllString(s, "")
	// A bare CR is how a TUI redraws a line in place; treating it as a newline
	// keeps each redraw on its own line for pattern matching.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

// remoteURLPattern matches the Remote Control link Claude Code prints. The
// character class stops at the first character that cannot be part of a URL, so
// a trailing bracket or full stop in the surrounding text is not swallowed.
var remoteURLPattern = regexp.MustCompile(`https://claude\.ai/code/[A-Za-z0-9_\-/?=&.%]+`)

// FindRemoteURL returns the first Remote Control URL in the text.
func FindRemoteURL(text string) string {
	m := remoteURLPattern.FindString(StripANSI(text))
	// A URL at the end of a sentence keeps the full stop otherwise.
	return strings.TrimRight(m, ".,")
}

var (
	trustPattern    = regexp.MustCompile(`(?i)\btrust\b[^\n]*\?`)
	enableRCPattern = regexp.MustCompile(`(?i)enable remote control\s*\??`)
	yesNoPattern    = regexp.MustCompile(`(?i)\[y/n\]|\(y/n\)|\[y/N\]|\(yes/no\)`)
	// A numbered-choice dialog, e.g. "1. Yes" / "2) No, and tell Claude".
	numberedOption = regexp.MustCompile(`(?m)^\s*(\d+)[.)]\s+(.{1,80}?)\s*$`)
	// Claude Code's permission dialogs open with a question of this shape.
	permissionPattern = regexp.MustCompile(`(?i)do you want to\b[^\n]*`)
)

// DetectPrompt looks for a question in the tail of a session's output.
//
// It is given a rolling window rather than the whole scrollback, so a prompt
// that was answered minutes ago is not detected again.
func DetectPrompt(window string) (Prompt, bool) {
	text := StripANSI(window)
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return Prompt{}, false
	}

	// Order matters: the specific prompts are checked before the generic
	// permission and yes/no shapes, which would also match them. Each detector
	// takes the *last* match, since a window can hold both the heading Claude
	// printed and the question line it is now waiting on.
	if m := lastMatch(enableRCPattern, text); m != "" {
		return Prompt{Kind: PromptEnableRC, Text: lastLineContaining(text, m), Options: []string{"y", "n"}}, true
	}
	if m := lastMatch(trustPattern, text); m != "" {
		return Prompt{Kind: PromptTrust, Text: lastLineContaining(text, m), Options: []string{"y", "n"}}, true
	}

	if m := lastMatch(permissionPattern, text); m != "" {
		p := Prompt{Kind: PromptPermission, Text: strings.TrimSpace(m)}
		if opts := numberedOptions(text); len(opts) > 0 {
			p.Options = opts
		} else if yesNoPattern.MatchString(text) {
			p.Options = []string{"y", "n"}
		}
		return p, true
	}

	// A yes/no question with no recognisable subject still needs answering.
	if yesNoPattern.MatchString(text) {
		return Prompt{Kind: PromptOther, Text: lastNonEmptyLine(text), Options: []string{"y", "n"}}, true
	}
	return Prompt{}, false
}

// numberedOptions collects the numbers of a numbered-choice dialog. The numbers
// are what gets sent back to the PTY, so the labels are only for display and
// are returned by DetectPrompt's Text.
func numberedOptions(text string) []string {
	matches := numberedOption.FindAllStringSubmatch(text, -1)
	if len(matches) < 2 {
		// A single "1." line is more likely prose than a dialog.
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, m[1])
	}
	return out
}

// lastMatch returns the final match of re in text, or "".
func lastMatch(re *regexp.Regexp, text string) string {
	all := re.FindAllString(text, -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1]
}

func lastLineContaining(text, needle string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], needle) {
			return strings.TrimSpace(lines[i])
		}
	}
	return strings.TrimSpace(needle)
}

func lastNonEmptyLine(text string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}

// KeyBytes maps the PWA's named keys to the bytes a terminal sends. The phone
// has no Esc or Ctrl key, so the key bar sends names instead of raw bytes
// (docs/04-API.md §5.2).
func KeyBytes(name string) ([]byte, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "enter", "return":
		return []byte("\r"), true
	case "esc", "escape":
		return []byte{0x1b}, true
	case "ctrl_c":
		return []byte{0x03}, true
	case "ctrl_d":
		return []byte{0x04}, true
	case "tab":
		return []byte("\t"), true
	case "backspace":
		return []byte{0x7f}, true
	case "up":
		return []byte("\x1b[A"), true
	case "down":
		return []byte("\x1b[B"), true
	case "right":
		return []byte("\x1b[C"), true
	case "left":
		return []byte("\x1b[D"), true
	default:
		return nil, false
	}
}
