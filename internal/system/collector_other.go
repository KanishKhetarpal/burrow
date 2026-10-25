//go:build !windows

package system

import (
	"os"
	"time"
)

// otherCollector reports what is portable, so the package builds and the HTTP
// layer can be tested off Windows. The agent only ships on Windows.
type otherCollector struct{ roots []string }

func newCollector(roots []string) Collector { return otherCollector{roots: roots} }

func (c otherCollector) Collect() (Info, error) {
	host, _ := os.Hostname()
	return Info{Hostname: host, OSVersion: "non-Windows", CheckedAt: time.Now().UTC()}, nil
}
