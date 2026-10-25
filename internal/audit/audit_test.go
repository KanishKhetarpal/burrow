package audit

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTemp(t *testing.T) (*Logger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l, path
}

func TestChainVerifies(t *testing.T) {
	l, path := openTemp(t)
	for i := 0; i < 5; i++ {
		if err := l.Log(Entry{Actor: "device:iphone", Action: ActionLogin, Result: ResultOK}); err != nil {
			t.Fatalf("Log: %v", err)
		}
	}
	res, err := Verify(path)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("chain invalid: %+v", res.Problems)
	}
	if res.Entries != 5 {
		t.Errorf("Entries = %d, want 5", res.Entries)
	}
}

func TestVerifyDetectsEditedLine(t *testing.T) {
	l, path := openTemp(t)
	for _, a := range []Action{ActionLogin, ActionFileDownload, ActionPowerShutdown} {
		if err := l.Log(Entry{Actor: "device:iphone", Action: a, Target: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	// Rewrite the middle entry's target, keeping its stored hash.
	lines := readLines(t, path)
	var e Entry
	if err := json.Unmarshal([]byte(lines[1]), &e); err != nil {
		t.Fatal(err)
	}
	e.Target = "something-else"
	tampered, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	lines[1] = string(tampered)
	writeLines(t, path, lines)

	res, err := Verify(path)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatal("Verify passed on an edited line")
	}
	if res.Problems[0].Line != 2 {
		t.Errorf("first problem on line %d, want 2", res.Problems[0].Line)
	}
	if !strings.Contains(res.Problems[0].Reason, "does not match its contents") {
		t.Errorf("reason = %q, want it to name the content mismatch", res.Problems[0].Reason)
	}
}

func TestVerifyDetectsDeletedLine(t *testing.T) {
	l, path := openTemp(t)
	for i := 0; i < 4; i++ {
		if err := l.Log(Entry{Actor: "device:iphone", Action: ActionFileDelete}); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	lines := readLines(t, path)
	// Drop the second entry: the third's prev hash now dangles.
	writeLines(t, path, append(lines[:1], lines[2:]...))

	res, err := Verify(path)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatal("Verify passed after a line was removed")
	}
	var sawLink bool
	for _, p := range res.Problems {
		if strings.Contains(p.Reason, "prev hash") || strings.Contains(p.Reason, "seq jumped") {
			sawLink = true
		}
	}
	if !sawLink {
		t.Errorf("problems = %+v, want a broken-link or seq-gap report", res.Problems)
	}
}

func TestLoggerResumesChainAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l1.Log(Entry{Action: ActionLogin}); err != nil {
		t.Fatal(err)
	}
	if err := l1.Log(Entry{Action: ActionLogin}); err != nil {
		t.Fatal(err)
	}
	l1.Close()

	l2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l2.Log(Entry{Action: ActionLogout}); err != nil {
		t.Fatal(err)
	}
	l2.Close()

	res, err := Verify(path)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("chain broken across restart: %+v", res.Problems)
	}
	entries, err := Read(path, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("Read returned %d entries, want 3", len(entries))
	}
	// Newest first, and the sequence must have continued rather than restarted.
	if entries[0].Seq != 3 || entries[0].Action != ActionLogout {
		t.Errorf("newest entry = seq %d %s, want seq 3 %s", entries[0].Seq, entries[0].Action, ActionLogout)
	}
}

func TestConcurrentAppendsSerialize(t *testing.T) {
	l, path := openTemp(t)
	const n = 60
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Log(Entry{Actor: "device:iphone", Action: ActionSessionInput}); err != nil {
				t.Errorf("Log: %v", err)
			}
		}()
	}
	wg.Wait()
	l.Close()

	res, err := Verify(path)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("concurrent appends broke the chain: %+v", res.Problems)
	}
	if res.Entries != n {
		t.Errorf("Entries = %d, want %d", res.Entries, n)
	}
}

func TestReadPagination(t *testing.T) {
	l, path := openTemp(t)
	for i := 0; i < 10; i++ {
		if err := l.Log(Entry{Action: ActionLogin}); err != nil {
			t.Fatal(err)
		}
	}
	page1, err := Read(path, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 3 || page1[0].Seq != 10 {
		t.Fatalf("page1 = %d entries starting at seq %d, want 3 starting at 10", len(page1), page1[0].Seq)
	}
	page2, err := Read(path, 3, page1[len(page1)-1].Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 3 || page2[0].Seq != 7 {
		t.Fatalf("page2 = %d entries starting at seq %d, want 3 starting at 7", len(page2), page2[0].Seq)
	}
}

func TestScrubRemovesSecrets(t *testing.T) {
	// The key-shaped cases are split so scripts/test.ps1's secret scan does
	// not flag the fixtures that exist to prove the scrubber catches them.
	cases := []string{
		"tskey-" + "auth-k123456789abcdefgh",
		"sk-" + "ant-api03-AAAAAAAAAAAAAAAAAAAA",
		"Authorization: Bearer abc.def.ghi",
		"password=hunter2",
		"ghp_" + "abcdefghijklmnopqrstuvwxyz0123",
	}
	for _, in := range cases {
		got := Scrub(in)
		if !strings.Contains(got, redacted) {
			t.Errorf("Scrub(%q) = %q, want it redacted", in, got)
		}
	}
	// Ordinary text must survive untouched, or the log becomes useless.
	plain := `session demo-app-2314 started in D:\code\demo-app`
	if got := Scrub(plain); got != plain {
		t.Errorf("Scrub mangled plain text: %q", got)
	}
}

func TestLogScrubsMetaAndTarget(t *testing.T) {
	l, path := openTemp(t)
	err := l.Log(Entry{
		Action: ActionConfigChange,
		Target: "token=tskey-" + "auth-k123456789abcdefgh",
		Meta:   map[string]any{"detail": "password=hunter2", "count": 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	l.Close()

	entries, err := Read(path, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	e := entries[0]
	if strings.Contains(e.Target, "tskey-") {
		t.Errorf("target kept a secret: %q", e.Target)
	}
	if s, _ := e.Meta["detail"].(string); strings.Contains(s, "hunter2") {
		t.Errorf("meta kept a secret: %q", s)
	}
	if n, _ := e.Meta["count"].(float64); n != 3 {
		t.Errorf("meta lost its non-string value: %+v", e.Meta)
	}
}

func TestLogFillsDefaults(t *testing.T) {
	l, path := openTemp(t)
	before := time.Now().UTC().Add(-time.Second)
	if err := l.Log(Entry{Action: ActionLogin}); err != nil {
		t.Fatal(err)
	}
	l.Close()

	entries, err := Read(path, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	e := entries[0]
	if e.Result != ResultOK {
		t.Errorf("Result = %q, want %q by default", e.Result, ResultOK)
	}
	if e.TS.Before(before) {
		t.Errorf("TS = %v, want a timestamp filled in at write time", e.TS)
	}
	if e.Seq != 1 {
		t.Errorf("Seq = %d, want 1", e.Seq)
	}
}

func TestVerifyMissingFileIsOK(t *testing.T) {
	res, err := Verify(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil {
		t.Fatalf("Verify on a missing log: %v", err)
	}
	if !res.OK || res.Entries != 0 {
		t.Errorf("res = %+v, want an empty, valid result", res)
	}
}
