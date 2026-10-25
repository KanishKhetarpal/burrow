package audit

import "regexp"

// The scrubber is a backstop, not the control. Callers are responsible for
// never handing secrets to the logger in the first place
// (docs/03-SECURITY.md T12); these patterns catch the mistakes.
var scrubPatterns = []*regexp.Regexp{
	// Tailscale auth keys and API tokens.
	regexp.MustCompile(`tskey-[A-Za-z0-9\-]+`),
	// Anthropic / OpenAI style bearer keys.
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{16,}`),
	// JWTs.
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`),
	// GitHub tokens.
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}`),
	// Anything that looks like a cookie or header assignment of a secret.
	regexp.MustCompile(`(?i)(authorization|cookie|password|passwd|secret|token|api[_\-]?key)\s*[:=]\s*\S+`),
	// PEM private key bodies.
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
}

const redacted = "[redacted]"

// defaultScrub replaces anything matching a secret pattern with a marker.
func defaultScrub(s string) string {
	if s == "" {
		return s
	}
	for _, re := range scrubPatterns {
		s = re.ReplaceAllString(s, redacted)
	}
	return s
}

// Scrub exposes the scrubber so the log writer and any diagnostic output apply
// the same filter.
func Scrub(s string) string { return defaultScrub(s) }
