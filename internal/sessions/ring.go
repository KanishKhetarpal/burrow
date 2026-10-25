package sessions

import "sync"

// ring is a fixed-size byte buffer that keeps the most recent bytes and drops
// the oldest.
//
// Scrollback is memory-only and capped per session (2 MiB by default), because
// PTY content must never reach the log or the disk: it holds whatever Claude
// printed, which can include file contents (docs/02-ARCHITECTURE.md §4).
type ring struct {
	mu   sync.RWMutex
	buf  []byte
	size int
	// start is the index of the oldest byte once the buffer has wrapped.
	start int
	full  bool
}

func newRing(size int) *ring {
	if size < 1024 {
		size = 1024
	}
	return &ring{buf: make([]byte, 0, size), size: size}
}

// Write appends p, discarding the oldest bytes if needed. It never fails, so it
// satisfies io.Writer for tee-ing PTY output.
func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// A write larger than the whole buffer: keep only its tail.
	if len(p) >= r.size {
		r.buf = r.buf[:r.size]
		copy(r.buf, p[len(p)-r.size:])
		r.start = 0
		r.full = true
		return len(p), nil
	}

	if !r.full {
		room := r.size - len(r.buf)
		if len(p) <= room {
			r.buf = append(r.buf, p...)
			if len(r.buf) == r.size {
				r.full = true
			}
			return len(p), nil
		}
		// Fill the remaining room, then wrap for the rest.
		r.buf = append(r.buf, p[:room]...)
		r.full = true
		p = p[room:]
		r.start = 0
	}

	// Wrapped: overwrite from start, advancing it past what was replaced.
	for _, b := range p {
		r.buf[r.start] = b
		r.start = (r.start + 1) % r.size
	}
	return len(p), nil
}

// Bytes returns the contents in order, oldest first.
func (r *ring) Bytes() []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.full {
		out := make([]byte, len(r.buf))
		copy(out, r.buf)
		return out
	}
	out := make([]byte, 0, r.size)
	out = append(out, r.buf[r.start:]...)
	out = append(out, r.buf[:r.start]...)
	return out
}

// Tail returns at most n bytes from the end, for the rolling detection window.
func (r *ring) Tail(n int) []byte {
	b := r.Bytes()
	if n <= 0 || n >= len(b) {
		return b
	}
	return b[len(b)-n:]
}

// Len reports how many bytes are held.
func (r *ring) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.full {
		return r.size
	}
	return len(r.buf)
}
