//go:build windows

package system

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modkernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = modkernel32.NewProc("GlobalMemoryStatusEx")
	procGetTickCount64       = modkernel32.NewProc("GetTickCount64")
	procGetSystemTimes       = modkernel32.NewProc("GetSystemTimes")
	procGetDiskFreeSpaceExW  = modkernel32.NewProc("GetDiskFreeSpaceExW")

	modntdll          = windows.NewLazySystemDLL("ntdll.dll")
	procRtlGetVersion = modntdll.NewProc("RtlGetVersion")
)

// memoryStatusEx mirrors MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// osVersionInfoEx mirrors RTL_OSVERSIONINFOEXW.
type osVersionInfoEx struct {
	OSVersionInfoSize uint32
	MajorVersion      uint32
	MinorVersion      uint32
	BuildNumber       uint32
	PlatformID        uint32
	CSDVersion        [128]uint16
	ServicePackMajor  uint16
	ServicePackMinor  uint16
	SuiteMask         uint16
	ProductType       uint8
	Reserved          uint8
}

// winCollector reads live stats.
type winCollector struct {
	// roots are the configured file roots, so the volumes reported are the ones
	// the owner actually cares about rather than every mounted drive.
	roots []string

	// CPU usage is a rate, so it needs two samples. The previous one is kept
	// here and the first call reports 0 rather than a meaningless figure.
	mu        sync.Mutex
	lastIdle  uint64
	lastTotal uint64
	haveLast  bool
}

func newCollector(roots []string) Collector { return &winCollector{roots: roots} }

// Collect gathers the stats for one API response.
func (c *winCollector) Collect() (Info, error) {
	info := Info{CheckedAt: time.Now().UTC()}

	host, err := os.Hostname()
	if err == nil {
		info.Hostname = host
	}
	info.OSVersion = osVersion()

	ticks, _, _ := procGetTickCount64.Call()
	info.UptimeSec = int64(time.Duration(ticks) * time.Millisecond / time.Second)

	var mem memoryStatusEx
	mem.Length = uint32(unsafe.Sizeof(mem))
	if r, _, errno := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&mem))); r == 0 {
		return info, fmt.Errorf("system: memory status: %w", errno)
	}
	info.RAMTotalMB = int64(mem.TotalPhys >> 20)
	info.RAMUsedMB = int64((mem.TotalPhys - mem.AvailPhys) >> 20)

	info.CPUPct = c.cpuPercent()
	info.Disks = c.disks()
	return info, nil
}

// cpuPercent computes usage since the previous call.
func (c *winCollector) cpuPercent() float64 {
	var idle, kernel, user windows.Filetime
	r, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r == 0 {
		return 0
	}
	// Kernel time already includes idle time, so total is kernel + user.
	idleT := filetimeToUint64(idle)
	totalT := filetimeToUint64(kernel) + filetimeToUint64(user)

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.haveLast {
		c.lastIdle, c.lastTotal, c.haveLast = idleT, totalT, true
		return 0
	}
	dIdle := idleT - c.lastIdle
	dTotal := totalT - c.lastTotal
	c.lastIdle, c.lastTotal = idleT, totalT
	if dTotal == 0 {
		return 0
	}
	pct := (1 - float64(dIdle)/float64(dTotal)) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	// One decimal place is as much precision as the home screen can use.
	return float64(int(pct*10)) / 10
}

// disks reports usage for the volumes the configured roots live on.
func (c *winCollector) disks() []Disk {
	seen := map[string]bool{}
	var out []Disk
	for _, root := range c.roots {
		vol := filepath.VolumeName(root)
		if vol == "" || seen[vol] {
			continue
		}
		seen[vol] = true

		path, err := windows.UTF16PtrFromString(vol + `\`)
		if err != nil {
			continue
		}
		var freeToCaller, total, free uint64
		r, _, _ := procGetDiskFreeSpaceExW.Call(
			uintptr(unsafe.Pointer(path)),
			uintptr(unsafe.Pointer(&freeToCaller)),
			uintptr(unsafe.Pointer(&total)),
			uintptr(unsafe.Pointer(&free)),
		)
		if r == 0 || total == 0 {
			continue
		}
		const gb = 1 << 30
		out = append(out, Disk{
			Mount:   vol,
			TotalGB: round1(float64(total) / gb),
			UsedGB:  round1(float64(total-free) / gb),
		})
	}
	return out
}

// osVersion reads the real version. RtlGetVersion is used rather than
// GetVersionEx, which lies for compatibility unless the binary is manifested.
func osVersion() string {
	var vi osVersionInfoEx
	vi.OSVersionInfoSize = uint32(unsafe.Sizeof(vi))
	if r, _, _ := procRtlGetVersion.Call(uintptr(unsafe.Pointer(&vi))); r != 0 {
		return "Windows"
	}
	// Windows 11 reports major 10 with a build of 22000 or more.
	name := "Windows 10"
	if vi.MajorVersion > 10 || (vi.MajorVersion == 10 && vi.BuildNumber >= 22000) {
		name = "Windows 11"
	}
	return fmt.Sprintf("%s (build %d)", name, vi.BuildNumber)
}

func filetimeToUint64(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
