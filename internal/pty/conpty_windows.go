//go:build windows

package pty

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// conPTY is the Windows implementation of Process.
type conPTY struct {
	console windows.Handle
	// inWrite is where keystrokes go; outRead is where output comes from.
	inWrite  *os.File
	outRead  *os.File
	inRead   windows.Handle
	outWrite windows.Handle

	proc   windows.Handle
	thread windows.Handle
	job    windows.Handle
	pid    int

	closeOnce sync.Once
	waitOnce  sync.Once
	exitCode  int
	waitErr   error
	done      chan struct{}
}

// Start launches cfg on a new pseudo-console.
func Start(cfg Config) (Process, error) {
	if cfg.Size.Cols == 0 || cfg.Size.Rows == 0 {
		cfg.Size = DefaultSize
	}

	p := &conPTY{done: make(chan struct{})}
	// Any failure after a handle is created must not leak it.
	ok := false
	defer func() {
		if !ok {
			p.Close()
		}
	}()

	// Two pipes: one for the console's input, one for its output. The handles
	// the console keeps (inRead, outWrite) are closed in the parent once the
	// child has started, or the pipe never reports EOF when the child exits.
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("pty: create input pipe: %w", err)
	}
	p.inRead = inRead
	p.inWrite = os.NewFile(uintptr(inWrite), "burrow-pty-in")

	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("pty: create output pipe: %w", err)
	}
	p.outWrite = outWrite
	p.outRead = os.NewFile(uintptr(outRead), "burrow-pty-out")

	var console windows.Handle
	size := windows.Coord{X: int16(cfg.Size.Cols), Y: int16(cfg.Size.Rows)}
	if err := windows.CreatePseudoConsole(size, inRead, outWrite, 0, &console); err != nil {
		return nil, fmt.Errorf("pty: create pseudo-console: %w", err)
	}
	p.console = console

	// A job object with KILL_ON_JOB_CLOSE makes stopping a session kill the
	// whole tree: Claude Code starts shells, and those start compilers
	// (docs/02-ARCHITECTURE.md §5).
	job, err := newKillOnCloseJob()
	if err != nil {
		return nil, err
	}
	p.job = job

	// The pseudo-console is handed to the child through a process attribute,
	// which is why the extended startup info form is needed.
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, fmt.Errorf("pty: attribute list: %w", err)
	}
	defer attrs.Delete()
	if err := attachPseudoConsole(attrs.List(), console); err != nil {
		return nil, err
	}

	si := new(windows.StartupInfoEx)
	si.ProcThreadAttributeList = attrs.List()
	si.Cb = uint32(unsafe.Sizeof(*si))

	// This is the part that is easy to get wrong and silent when wrong. A
	// console process receives its standard handles through the PEB, whatever
	// the inheritHandles argument says. Without the three lines below the child
	// is correctly attached to the pseudo-console — `mode con` inside it
	// reports the size asked of CreatePseudoConsole — yet its stdout still
	// points at the parent's stdout, so everything it prints bypasses the PTY
	// and the output pipe carries only the console's own handshake bytes.
	// Declaring the std handles present but NULL leaves the child nothing to
	// inherit, so it falls back to its attached console, which is the PTY.
	si.Flags |= windows.STARTF_USESTDHANDLES
	si.StdInput, si.StdOutput, si.StdErr = 0, 0, 0

	var token windows.Token
	if cfg.Restricted {
		token, err = RestrictedToken()
		if err != nil {
			return nil, err
		}
		defer token.Close()
	}

	envBlock, freeEnv, err := environmentBlock(token, cfg.Env)
	if err != nil {
		return nil, err
	}
	defer freeEnv()

	cmdLine, err := buildCommandLine(cfg, envBlock)
	if err != nil {
		return nil, err
	}
	cmdLine16, err := windows.UTF16PtrFromString(cmdLine)
	if err != nil {
		return nil, fmt.Errorf("pty: encode command line: %w", err)
	}
	var dir16 *uint16
	if cfg.Dir != "" {
		if dir16, err = windows.UTF16PtrFromString(cfg.Dir); err != nil {
			return nil, fmt.Errorf("pty: encode working directory: %w", err)
		}
	}

	// CREATE_UNICODE_ENVIRONMENT is required because the environment block is
	// UTF-16. CREATE_SUSPENDED lets the process be put in the job before it
	// runs, so a fast-forking child cannot escape the job.
	//
	// CREATE_NO_WINDOW, DETACHED_PROCESS and CREATE_NEW_CONSOLE are all absent
	// deliberately: each one stops the pseudo-console attaching, and the only
	// symptom is an output pipe that never carries anything.
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT |
		windows.CREATE_UNICODE_ENVIRONMENT |
		windows.CREATE_SUSPENDED)

	var pi windows.ProcessInformation
	if cfg.Restricted {
		err = windows.CreateProcessAsUser(token, nil, cmdLine16, nil, nil, false,
			flags, envBlock, dir16, &si.StartupInfo, &pi)
		if err != nil {
			// CreateProcessAsUser needs SeIncreaseQuotaPrivilege, which a
			// service running as the user may not hold. Rather than silently
			// dropping the restriction, say so: running Claude with the
			// service's full admin token is exactly what D5 forbids.
			return nil, fmt.Errorf("pty: start restricted process (check SeIncreaseQuotaPrivilege): %w", err)
		}
	} else {
		err = windows.CreateProcess(nil, cmdLine16, nil, nil, false,
			flags, envBlock, dir16, &si.StartupInfo, &pi)
		if err != nil {
			return nil, fmt.Errorf("pty: start process: %w", err)
		}
	}
	p.proc, p.thread, p.pid = pi.Process, pi.Thread, int(pi.ProcessId)

	if err := windows.AssignProcessToJobObject(p.job, p.proc); err != nil {
		// Without the job, stopping a session would leave orphans behind.
		windows.TerminateProcess(p.proc, 1)
		return nil, fmt.Errorf("pty: assign to job: %w", err)
	}
	if _, err := windows.ResumeThread(p.thread); err != nil {
		windows.TerminateProcess(p.proc, 1)
		return nil, fmt.Errorf("pty: resume child: %w", err)
	}

	// The console owns its ends of the pipes now. Closing the parent's copies
	// is what lets a read on outRead return EOF when the child exits.
	windows.CloseHandle(p.inRead)
	p.inRead = 0
	windows.CloseHandle(p.outWrite)
	p.outWrite = 0

	ok = true
	return p, nil
}

var (
	modkernel32                   = windows.NewLazySystemDLL("kernel32.dll")
	procUpdateProcThreadAttribute = modkernel32.NewProc("UpdateProcThreadAttribute")
)

// attachPseudoConsole adds the PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE attribute.
//
// The attribute's lpValue is the HPCON handle itself, passed by value rather
// than as a pointer to it. The typed wrapper in x/sys takes an unsafe.Pointer,
// which would mean converting a handle into a pointer — something `go vet`
// rightly objects to — so the call is made directly with a uintptr argument.
func attachPseudoConsole(list *windows.ProcThreadAttributeList, console windows.Handle) error {
	r, _, errno := procUpdateProcThreadAttribute.Call(
		uintptr(unsafe.Pointer(list)),
		0, // reserved flags
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		uintptr(console),
		unsafe.Sizeof(console),
		0, 0, // lpPreviousValue, lpReturnSize
	)
	if r == 0 {
		return fmt.Errorf("pty: attach pseudo-console: %w", errno)
	}
	return nil
}

// buildCommandLine resolves the executable against the child's PATH and quotes
// the arguments.
//
// The child's PATH is used rather than the service's: the service environment
// may not include the user's npm or Git directories, and `claude` lives in one
// of those.
func buildCommandLine(cfg Config, envBlock *uint16) (string, error) {
	if strings.TrimSpace(cfg.Path) == "" {
		return "", errors.New("pty: no executable")
	}
	exe := cfg.Path
	if !filepath.IsAbs(exe) {
		env := parseEnvBlock(envBlock)
		pathVal, _ := lookupEnv(env, "PATH")
		pathExt, ok := lookupEnv(env, "PATHEXT")
		if !ok {
			pathExt = ".COM;.EXE;.BAT;.CMD"
		}
		resolved, err := lookPathIn(exe, pathVal, pathExt)
		if err != nil {
			return "", err
		}
		exe = resolved
	}
	parts := append([]string{exe}, cfg.Args...)
	line := windows.ComposeCommandLine(parts)

	// A .cmd or .bat is a script for cmd.exe, not an executable, and
	// CreateProcess will not run one directly. npm installs `claude` as
	// claude.cmd, so this is the normal case, not an edge one. /s keeps cmd
	// from stripping the quotes around a path with spaces in it.
	switch strings.ToLower(filepath.Ext(exe)) {
	case ".cmd", ".bat":
		env := parseEnvBlock(envBlock)
		comspec, ok := lookupEnv(env, "ComSpec")
		if !ok || comspec == "" {
			comspec = `C:\Windows\System32\cmd.exe`
		}
		line = windows.EscapeArg(comspec) + ` /d /s /c "` + line + `"`
	}
	return line, nil
}

// lookPathIn resolves name against an explicit PATH, which exec.LookPath cannot
// do (it only ever reads the current process's own).
func lookPathIn(name, pathVal, pathExt string) (string, error) {
	// A bare name is only tried when it already has an extension. npm puts an
	// extensionless `claude` next to claude.cmd — a shell script for Git Bash —
	// and trying the bare name first picked that, which CreateProcess cannot
	// run, so every new chat failed.
	exts := []string{""}
	if filepath.Ext(name) == "" {
		exts = nil
		for _, e := range strings.Split(pathExt, ";") {
			if e = strings.TrimSpace(e); e != "" {
				exts = append(exts, e)
			}
		}
	}
	for _, dir := range strings.Split(pathVal, ";") {
		if dir = strings.TrimSpace(dir); dir == "" {
			continue
		}
		for _, ext := range exts {
			candidate := filepath.Join(dir, name+ext)
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("pty: %q not found on the child's PATH", name)
}

// newKillOnCloseJob creates a job that terminates its members when the last
// handle to it closes.
func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("pty: create job: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("pty: configure job: %w", err)
	}
	return job, nil
}

func (p *conPTY) Read(b []byte) (int, error)  { return p.outRead.Read(b) }
func (p *conPTY) Write(b []byte) (int, error) { return p.inWrite.Write(b) }
func (p *conPTY) PID() int                    { return p.pid }

func (p *conPTY) Resize(s Size) error {
	if s.Cols == 0 || s.Rows == 0 {
		return errors.New("pty: resize to zero")
	}
	if p.console == 0 {
		return errors.New("pty: console is closed")
	}
	if err := windows.ResizePseudoConsole(p.console, windows.Coord{X: int16(s.Cols), Y: int16(s.Rows)}); err != nil {
		return fmt.Errorf("pty: resize: %w", err)
	}
	return nil
}

// Wait blocks for the child and reports its exit code. It is safe to call from
// several goroutines; later callers get the recorded result.
func (p *conPTY) Wait() (int, error) {
	p.waitOnce.Do(func() {
		defer close(p.done)
		if p.proc == 0 {
			p.waitErr = errors.New("pty: no process")
			return
		}
		ev, err := windows.WaitForSingleObject(p.proc, windows.INFINITE)
		if err != nil {
			p.waitErr = fmt.Errorf("pty: wait: %w", err)
			return
		}
		if ev != windows.WAIT_OBJECT_0 {
			p.waitErr = fmt.Errorf("pty: unexpected wait result %d", ev)
			return
		}
		var code uint32
		if err := windows.GetExitCodeProcess(p.proc, &code); err != nil {
			p.waitErr = fmt.Errorf("pty: exit code: %w", err)
			return
		}
		p.exitCode = int(code)
	})
	<-p.done
	return p.exitCode, p.waitErr
}

// Kill terminates the whole tree by closing the job, then makes sure the direct
// child is gone.
func (p *conPTY) Kill() error {
	var firstErr error
	if p.job != 0 {
		if err := windows.TerminateJobObject(p.job, 1); err != nil {
			firstErr = fmt.Errorf("pty: terminate job: %w", err)
		}
	}
	if p.proc != 0 {
		if err := windows.TerminateProcess(p.proc, 1); err != nil &&
			!errors.Is(err, syscall.Errno(windows.ERROR_ACCESS_DENIED)) && firstErr == nil {
			// ACCESS_DENIED here normally means it already exited.
			firstErr = fmt.Errorf("pty: terminate process: %w", err)
		}
	}
	return firstErr
}

// Close releases every handle. Closing the pseudo-console signals the child
// that its console went away, so a well-behaved program exits on its own.
//
// The order matters: ClosePseudoConsole blocks until the console host has
// flushed its pending output, so if the read end is still open and nobody is
// draining it, Close deadlocks. Closing the pipe ends first makes those writes
// fail immediately instead.
func (p *conPTY) Close() error {
	p.closeOnce.Do(func() {
		if p.inWrite != nil {
			p.inWrite.Close()
		}
		if p.outRead != nil {
			p.outRead.Close()
		}
		if p.console != 0 {
			windows.ClosePseudoConsole(p.console)
			p.console = 0
		}
		for _, h := range []*windows.Handle{&p.inRead, &p.outWrite, &p.thread, &p.proc, &p.job} {
			if *h != 0 {
				windows.CloseHandle(*h)
				*h = 0
			}
		}
	})
	return nil
}

// utf16BlockToStrings walks a double-NUL-terminated UTF-16 block.
func utf16BlockToStrings(block *uint16) []string {
	if block == nil {
		return nil
	}
	var out []string
	p := unsafe.Pointer(block)
	for {
		// Measure this entry.
		n := 0
		for *(*uint16)(unsafe.Add(p, uintptr(n)*2)) != 0 {
			n++
		}
		if n == 0 {
			return out // the empty entry ends the block
		}
		s := windows.UTF16ToString(unsafe.Slice((*uint16)(p), n))
		out = append(out, s)
		p = unsafe.Add(p, uintptr(n+1)*2)
	}
}
