// Package system reports machine stats for the PWA's home screen.
package system

import (
	"runtime"
	"time"
)

// Info is the payload of GET /api/system (docs/04-API.md §3).
type Info struct {
	Hostname   string    `json:"hostname"`
	OSVersion  string    `json:"osVersion"`
	UptimeSec  int64     `json:"uptimeSec"`
	CPUPct     float64   `json:"cpuPct"`
	RAMUsedMB  int64     `json:"ramUsedMB"`
	RAMTotalMB int64     `json:"ramTotalMB"`
	Disks      []Disk    `json:"disks"`
	CheckedAt  time.Time `json:"checkedAt"`
}

// Disk is one volume's usage.
type Disk struct {
	Mount   string  `json:"mount"`
	UsedGB  float64 `json:"usedGB"`
	TotalGB float64 `json:"totalGB"`
}

// Collector gathers stats. The interface exists so the HTTP handler can be
// tested without reading the real machine.
type Collector interface {
	Collect() (Info, error)
}

// New returns the platform collector.
func New(roots []string) Collector { return newCollector(roots) }

// Static is a fixed collector for tests.
type Static struct{ Info Info }

// Collect implements Collector.
func (s Static) Collect() (Info, error) { return s.Info, nil }

// goVersion is reported in the About screen alongside the agent version.
func goVersion() string { return runtime.Version() }
