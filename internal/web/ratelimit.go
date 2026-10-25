package web

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"burrow/internal/audit"
	"burrow/internal/identity"
)

// Rate limits from docs/03-SECURITY.md §4.
const (
	AuthRatePerMin = 10
	APIRatePerMin  = 120
)

// Limiter is a fixed-window counter per key. A token bucket would be smoother,
// but a per-minute window is what the spec states and is easier to reason about
// when reading the audit log.
type Limiter struct {
	mu      sync.Mutex
	windows map[string]*window
	limit   int
	period  time.Duration
	now     func() time.Time
}

type window struct {
	count int
	reset time.Time
}

// NewLimiter builds a limiter allowing limit requests per period.
func NewLimiter(limit int, period time.Duration) *Limiter {
	return &Limiter{
		windows: map[string]*window{},
		limit:   limit,
		period:  period,
		now:     time.Now,
	}
}

// Allow records a hit for key and reports whether it is within the limit.
// The second return is how long until the window resets.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	w, ok := l.windows[key]
	if !ok || now.After(w.reset) {
		// Sweep opportunistically: without it, a long-running agent would keep
		// a window for every key it has ever seen.
		if len(l.windows) > 1024 {
			for k, v := range l.windows {
				if now.After(v.reset) {
					delete(l.windows, k)
				}
			}
		}
		w = &window{reset: now.Add(l.period)}
		l.windows[key] = w
	}
	w.count++
	if w.count > l.limit {
		return false, w.reset.Sub(now)
	}
	return true, w.reset.Sub(now)
}

// RateLimit wraps a handler with a per-node limiter. Keying on the node's
// StableID rather than the IP means a device cannot get a fresh budget by
// picking a new tailnet address.
func RateLimit(l *Limiter, auditFn func(audit.Entry)) func(http.Handler) http.Handler {
	if auditFn == nil {
		auditFn = func(audit.Entry) {}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := "ip:" + identity.HostOnly(r.RemoteAddr)
			if n, ok := NodeFrom(r.Context()); ok {
				key = "node:" + n.StableID
			}
			ok, retryIn := l.Allow(key)
			if !ok {
				secs := int(retryIn.Seconds())
				if secs < 1 {
					secs = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(secs))
				auditFn(audit.Entry{
					Actor:  key,
					Action: audit.ActionDenyRateLimit,
					Target: r.URL.Path,
					Result: audit.ResultDenied,
					IP:     identity.HostOnly(r.RemoteAddr),
				})
				WriteError(w, http.StatusTooManyRequests, CodeRateLimited, "too many requests")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
