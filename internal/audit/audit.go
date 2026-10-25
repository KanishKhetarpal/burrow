// Package audit writes a tamper-evident, append-only event log.
//
// Each line carries the SHA-256 of the previous line, so editing or deleting a
// line anywhere in the file breaks the chain from that point on and `burrow
// audit verify` reports exactly where (docs/03-SECURITY.md T13).
package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Action names every audited event (docs/04-API.md §9). Using a named type
// keeps typos out of the log, where they would be invisible until an audit.
type Action string

const (
	ActionPair      Action = "auth.pair"
	ActionLogin     Action = "auth.login"
	ActionLoginFail Action = "auth.login_fail"
	ActionStepup    Action = "auth.stepup"
	ActionLogout    Action = "auth.logout"

	ActionDenyIdentity  Action = "deny.identity"
	ActionDenyCSRF      Action = "deny.csrf"
	ActionDenyRateLimit Action = "deny.ratelimit"
	ActionDenyStepup    Action = "deny.stepup"

	ActionWakeRequest Action = "wake.request"

	ActionSessionStart  Action = "session.start"
	ActionSessionStop   Action = "session.stop"
	ActionSessionResume Action = "session.resume"
	ActionSessionKillAl Action = "session.kill_all"
	ActionSessionInput  Action = "session.input"

	ActionFileDownload Action = "file.download"
	ActionFileUpload   Action = "file.upload"
	ActionFileMkdir    Action = "file.mkdir"
	ActionFileRename   Action = "file.rename"
	ActionFileDelete   Action = "file.delete"

	ActionPowerSleep     Action = "power.sleep"
	ActionPowerHibernate Action = "power.hibernate"
	ActionPowerShutdown  Action = "power.shutdown"
	ActionPowerRestart   Action = "power.restart"
	ActionPowerLock      Action = "power.lock"
	ActionPowerKeepAwake Action = "power.keep_awake"

	ActionLockdownEnter Action = "lockdown.enter"
	ActionLockdownExit  Action = "lockdown.exit"

	ActionSSHKeyAdd    Action = "sshkey.add"
	ActionDeviceRevoke Action = "device.revoke"
	ActionConfigChange Action = "config.change"
	ActionAppOpen      Action = "app.open"
)

// Result is the outcome recorded on an entry.
type Result string

const (
	ResultOK     Result = "ok"
	ResultDenied Result = "denied"
	ResultError  Result = "error"
)

// Entry is one line of the log. Field order matters: hashing uses the
// marshalled form of the entry with Hash blanked, so the struct tag order below
// is part of the on-disk contract.
type Entry struct {
	Seq    int64          `json:"seq"`
	TS     time.Time      `json:"ts"`
	Actor  string         `json:"actor"`
	Action Action         `json:"action"`
	Target string         `json:"target,omitempty"`
	Result Result         `json:"result"`
	IP     string         `json:"ip,omitempty"`
	Meta   map[string]any `json:"meta,omitempty"`
	Prev   string         `json:"prev"`
	Hash   string         `json:"hash"`
}

// Logger appends entries under a mutex, so concurrent handlers serialise
// instead of interleaving partial lines.
type Logger struct {
	mu      sync.Mutex
	path    string
	f       *os.File
	seq     int64
	lastSum string
	scrub   func(string) string
}

// Open opens (or creates) the log at path and reads the tail to recover the
// sequence number and last hash, so restarts continue the same chain.
func Open(path string) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("audit dir: %w", err)
	}
	seq, last, err := tail(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	return &Logger{path: path, f: f, seq: seq, lastSum: last, scrub: defaultScrub}, nil
}

// tail reads the existing log to find the highest seq and its hash.
func tail(path string) (seq int64, lastHash string, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("read audit log: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			// A truncated final line (power cut mid-write) must not stop the
			// agent from logging; verify will report it.
			continue
		}
		seq, lastHash = e.Seq, e.Hash
	}
	if err := sc.Err(); err != nil {
		return 0, "", fmt.Errorf("scan audit log: %w", err)
	}
	return seq, lastHash, nil
}

// Close flushes and closes the underlying file.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Log appends one entry. It never returns the written entry's secrets because
// it holds none: callers must not put tokens, cookies or PTY content in Meta,
// and string values pass through the scrubber as a backstop.
func (l *Logger) Log(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return errors.New("audit log is closed")
	}

	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	} else {
		e.TS = e.TS.UTC()
	}
	if e.Result == "" {
		e.Result = ResultOK
	}
	e.Seq = l.seq + 1
	e.Prev = l.lastSum
	e.Actor = l.scrub(e.Actor)
	e.Target = l.scrub(e.Target)
	for k, v := range e.Meta {
		if s, ok := v.(string); ok {
			e.Meta[k] = l.scrub(s)
		}
	}

	line, sum, err := seal(e)
	if err != nil {
		return err
	}
	if _, err := l.f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write audit entry: %w", err)
	}
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("sync audit entry: %w", err)
	}
	l.seq = e.Seq
	l.lastSum = sum
	return nil
}

// seal computes the entry hash and returns the line to write.
//
// The hash covers the entry with Hash emptied, so a verifier can recompute it
// from the stored line alone.
func seal(e Entry) (line []byte, sum string, err error) {
	e.Hash = ""
	body, err := json.Marshal(e)
	if err != nil {
		return nil, "", fmt.Errorf("marshal audit entry: %w", err)
	}
	h := sha256.Sum256(body)
	sum = hex.EncodeToString(h[:])
	e.Hash = sum
	line, err = json.Marshal(e)
	if err != nil {
		return nil, "", fmt.Errorf("marshal audit entry: %w", err)
	}
	return line, sum, nil
}

// VerifyResult reports the outcome of a chain check.
type VerifyResult struct {
	Entries  int
	OK       bool
	Problems []Problem
}

// Problem is one broken link, in file order.
type Problem struct {
	Line   int
	Seq    int64
	Reason string
}

// Verify walks the log and checks every hash and link.
func Verify(path string) (VerifyResult, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return VerifyResult{OK: true}, nil
	}
	if err != nil {
		return VerifyResult{}, fmt.Errorf("open audit log: %w", err)
	}
	defer f.Close()
	return VerifyReader(f)
}

// VerifyReader is Verify over an arbitrary reader, so tests need no files.
func VerifyReader(r io.Reader) (VerifyResult, error) {
	var (
		res      VerifyResult
		prevHash string
		prevSeq  int64
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		res.Entries++

		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			res.Problems = append(res.Problems, Problem{Line: lineNo, Reason: "not valid JSON"})
			continue
		}

		stored := e.Hash
		_, recomputed, err := seal(e)
		if err != nil {
			res.Problems = append(res.Problems, Problem{Line: lineNo, Seq: e.Seq, Reason: "cannot re-hash"})
			continue
		}
		if stored != recomputed {
			res.Problems = append(res.Problems, Problem{
				Line: lineNo, Seq: e.Seq,
				Reason: "entry hash does not match its contents (line edited)",
			})
		}
		if e.Prev != prevHash {
			res.Problems = append(res.Problems, Problem{
				Line: lineNo, Seq: e.Seq,
				Reason: "prev hash does not match the previous line (line inserted or removed)",
			})
		}
		if prevSeq != 0 && e.Seq != prevSeq+1 {
			res.Problems = append(res.Problems, Problem{
				Line: lineNo, Seq: e.Seq,
				Reason: fmt.Sprintf("seq jumped from %d to %d", prevSeq, e.Seq),
			})
		}
		prevHash, prevSeq = stored, e.Seq
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("scan audit log: %w", err)
	}
	sort.SliceStable(res.Problems, func(i, j int) bool { return res.Problems[i].Line < res.Problems[j].Line })
	res.OK = len(res.Problems) == 0
	return res, nil
}

// Read returns up to limit entries, newest first, optionally only those with
// Seq below before (0 = from the end). Used by GET /api/audit.
func Read(path string, limit int, before int64) ([]Entry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	defer f.Close()

	// The log is append-only and bounded by use (a handful of entries per
	// action), so a forward scan keeping a ring of the last `limit` matches is
	// simpler and fast enough compared with reverse block reads.
	ring := make([]Entry, 0, limit)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			continue
		}
		if before > 0 && e.Seq >= before {
			continue
		}
		if len(ring) == limit {
			ring = ring[1:]
		}
		ring = append(ring, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan audit log: %w", err)
	}
	// Reverse into newest-first order.
	out := make([]Entry, 0, len(ring))
	for i := len(ring) - 1; i >= 0; i-- {
		out = append(out, ring[i])
	}
	return out, nil
}
