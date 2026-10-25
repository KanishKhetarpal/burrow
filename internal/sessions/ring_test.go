package sessions

import (
	"strings"
	"sync"
	"testing"
)

func TestRingKeepsTheNewestBytes(t *testing.T) {
	r := newRing(2048)
	r.Write([]byte("hello "))
	r.Write([]byte("world"))
	if got := string(r.Bytes()); got != "hello world" {
		t.Fatalf("Bytes = %q, want %q", got, "hello world")
	}
	if r.Len() != 11 {
		t.Errorf("Len = %d, want 11", r.Len())
	}
}

func TestRingDropsTheOldestWhenFull(t *testing.T) {
	// The minimum size is 1024, so test wrapping at that size.
	r := newRing(1024)
	r.Write([]byte(strings.Repeat("a", 1000)))
	r.Write([]byte(strings.Repeat("b", 100)))

	got := r.Bytes()
	if len(got) != 1024 {
		t.Fatalf("length = %d, want the buffer size 1024", len(got))
	}
	// 76 a's dropped, so the content is 924 a's then 100 b's.
	if want := strings.Repeat("a", 924) + strings.Repeat("b", 100); string(got) != want {
		t.Errorf("Bytes = %q…%q, want 924 a's then 100 b's",
			string(got[:20]), string(got[len(got)-20:]))
	}
}

func TestRingHandlesAWriteBiggerThanItself(t *testing.T) {
	r := newRing(1024)
	r.Write([]byte("xx"))
	r.Write([]byte(strings.Repeat("y", 1500) + "TAIL"))

	got := r.Bytes()
	if len(got) != 1024 {
		t.Fatalf("length = %d, want 1024", len(got))
	}
	if !strings.HasSuffix(string(got), "TAIL") {
		t.Errorf("the tail of an oversized write was lost: %q", string(got[len(got)-10:]))
	}
	if strings.Contains(string(got), "xx") {
		t.Error("an oversized write did not displace the earlier content")
	}
}

func TestRingTail(t *testing.T) {
	r := newRing(2048)
	r.Write([]byte("abcdefghij"))
	if got := string(r.Tail(3)); got != "hij" {
		t.Errorf("Tail(3) = %q, want %q", got, "hij")
	}
	if got := string(r.Tail(100)); got != "abcdefghij" {
		t.Errorf("Tail(100) = %q, want everything", got)
	}
	if got := string(r.Tail(0)); got != "abcdefghij" {
		t.Errorf("Tail(0) = %q, want everything", got)
	}
}

func TestRingIsSafeUnderConcurrentWrites(t *testing.T) {
	r := newRing(8192)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r.Write([]byte("0123456789"))
			}
		}()
	}
	// Read while writing, which is what the detector does.
	for i := 0; i < 50; i++ {
		_ = r.Bytes()
	}
	wg.Wait()
	if r.Len() != 8192 {
		t.Errorf("Len = %d, want the buffer full at 8192", r.Len())
	}
}
