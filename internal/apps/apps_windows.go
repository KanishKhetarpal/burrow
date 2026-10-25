//go:build windows

package apps

import (
	"encoding/csv"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"burrow/internal/config"
)

// Manager is the Windows Opener.
type Manager struct {
	mu    sync.Mutex
	known map[string]string // app name (lower) -> task name
	// run executes schtasks; replaced in tests.
	run func(args ...string) ([]byte, error)
	// desktop reports whether anyone is logged in; replaced in tests.
	desktop func() bool
}

// New builds a Manager that drives the real schtasks.exe.
func New() *Manager {
	return &Manager{known: map[string]string{}, run: schtasks, desktop: activeSession}
}

func schtasks(args ...string) ([]byte, error) {
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, err
	}
	out, err := exec.Command(filepath.Join(dir, "schtasks.exe"), args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("schtasks %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Sync implements Opener.
func (m *Manager) Sync(list []config.App) error {
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("apps: current user: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	want := map[string]bool{}
	m.known = map[string]string{}
	var errs []string
	for _, a := range list {
		name := TaskName(a.Name)
		want[strings.ToLower(name)] = true
		m.known[strings.ToLower(a.Name)] = name
		if err := m.register(name, u.Username, a); err != nil {
			errs = append(errs, err.Error())
		}
	}

	// Remove tasks for apps taken out of burrow.yaml, so a button that no
	// longer exists cannot be run by name either.
	if out, err := m.run("/query", "/fo", "csv", "/nh"); err == nil {
		rows, _ := csv.NewReader(strings.NewReader(string(out))).ReadAll()
		prefix := strings.ToLower(taskFolder + "Open ")
		for _, row := range rows {
			if len(row) == 0 {
				continue
			}
			tn := row[0]
			if strings.HasPrefix(strings.ToLower(tn), prefix) && !want[strings.ToLower(tn)] {
				_, _ = m.run("/delete", "/tn", tn, "/f")
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("apps: %s", strings.Join(errs, "; "))
	}
	return nil
}

// register writes one task. schtasks reads /xml files as UTF-16.
func (m *Manager) register(task, userName string, a config.App) error {
	def := taskXML(userName, a.Path, windows.ComposeCommandLine(a.Args))
	f, err := os.CreateTemp("", "burrow-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	units := utf16.Encode([]rune(def))
	buf := make([]byte, 2+2*len(units))
	buf[0], buf[1] = 0xFF, 0xFE // UTF-16LE byte-order mark
	for i, u := range units {
		buf[2+2*i] = byte(u)
		buf[3+2*i] = byte(u >> 8)
	}
	if _, err := f.Write(buf); err != nil {
		f.Close()
		return err
	}
	f.Close()
	_, err = m.run("/create", "/tn", task, "/xml", f.Name(), "/f")
	return err
}

// Open implements Opener.
func (m *Manager) Open(name string) error {
	m.mu.Lock()
	task, ok := m.known[strings.ToLower(name)]
	m.mu.Unlock()
	if !ok {
		return ErrUnknown
	}
	if !m.desktop() {
		return ErrNoDesktop
	}
	_, err := m.run("/run", "/tn", task)
	return err
}

// activeSession reports whether any Windows session is logged in and active,
// at the console or over Remote Desktop.
func activeSession() bool {
	var sessions *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &sessions, &count); err != nil {
		// Unknown: let the task try rather than refuse.
		return true
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessions)))
	for _, s := range unsafe.Slice(sessions, count) {
		if s.State == windows.WTSActive && s.SessionID != 0 {
			return true
		}
	}
	return false
}
