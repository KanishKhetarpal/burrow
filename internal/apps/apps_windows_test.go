//go:build windows

package apps

import (
	"errors"
	"strings"
	"testing"

	"burrow/internal/config"
)

func TestTaskNameIsSafeAndInTheBurrowFolder(t *testing.T) {
	if got := TaskName("Chrome · Personal <work>"); got != `\Burrow\Open Chrome Personal work` {
		t.Fatalf("TaskName = %q", got)
	}
}

func TestSyncRegistersEachAppAndOpenRunsIt(t *testing.T) {
	var calls []string
	m := New()
	m.run = func(args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}
	m.desktop = func() bool { return true }

	err := m.Sync([]config.App{{Name: "Chrome · Personal", Path: `C:\chrome.exe`, Args: []string{"--profile-directory=Profile 2"}}})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !strings.HasPrefix(calls[0], `/create /tn \Burrow\Open Chrome Personal /xml `) {
		t.Fatalf("first call = %q", calls[0])
	}

	if err := m.Open("chrome · personal"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if last := calls[len(calls)-1]; last != `/run /tn \Burrow\Open Chrome Personal` {
		t.Fatalf("open call = %q", last)
	}
	if err := m.Open("nope"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown app err = %v", err)
	}
	m.desktop = func() bool { return false }
	if err := m.Open("Chrome · Personal"); !errors.Is(err, ErrNoDesktop) {
		t.Fatalf("no desktop err = %v", err)
	}
}

func TestTaskXMLEscapesArguments(t *testing.T) {
	x := taskXML(`PC\pcuser`, `C:\Program Files\Google\Chrome\Application\chrome.exe`, `--profile-directory="Profile 2" --x=a&b`)
	for _, want := range []string{
		`<UserId>PC\pcuser</UserId>`,
		`<LogonType>InteractiveToken</LogonType>`,
		`<RunLevel>LeastPrivilege</RunLevel>`,
		`--profile-directory=&quot;Profile 2&quot; --x=a&amp;b`,
	} {
		if !strings.Contains(x, want) {
			t.Errorf("task XML is missing %q", want)
		}
	}
}
