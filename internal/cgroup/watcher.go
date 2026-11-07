package cgroup

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/motilayo/gpu-gatekeeper/internal/bpf"
)

// QuotaSpec defines the rate limiting policy for a target cgroup path or glob.
type QuotaSpec struct {
	Target       string
	RefillRate   uint64
	MaxBurst     uint64
	MaxVramBytes uint64
	VramPolicy   uint64
}

// ResolveInode returns the inode number for the given file or directory path.
func ResolveInode(path string) (uint64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("failed to cast stat for %s to *syscall.Stat_t", path)
	}
	return uint64(stat.Ino), nil
}

// NormalizeCgroupPath ensures the target is an absolute path, defaulting to /sys/fs/cgroup.
func NormalizeCgroupPath(target string) string {
	if strings.HasPrefix(target, "/") {
		return target
	}
	return filepath.Join("/sys/fs/cgroup", target)
}

// Watcher monitors cgroup v2 directories matching specified paths or globs
// and dynamically enrolls new cgroups into the eBPF quota map.
type Watcher struct {
	quotaMap *ebpf.Map
	specs    []QuotaSpec
	mu       sync.Mutex
	enrolled map[uint64]bool
}

// NewWatcher creates a new cgroup watcher.
func NewWatcher(quotaMap *ebpf.Map, specs []QuotaSpec) *Watcher {
	return &Watcher{
		quotaMap: quotaMap,
		specs:    specs,
		enrolled: make(map[uint64]bool),
	}
}

// SyncOnce scans the filesystem for matching cgroups and enrolls any new ones.
func (w *Watcher) SyncOnce() {
	for _, spec := range w.specs {
		pattern := NormalizeCgroupPath(spec.Target)
		matches, err := filepath.Glob(pattern)
		if err != nil {
			log.Printf("cgroup watcher: glob error for pattern %q: %v", pattern, err)
			continue
		}

		for _, match := range matches {
			info, err := os.Stat(match)
			if err != nil {
				continue
			}
			if !info.IsDir() {
				continue
			}

			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				continue
			}
			inode := uint64(stat.Ino)

			w.mu.Lock()
			if w.enrolled[inode] {
				w.mu.Unlock()
				continue
			}

			state := bpf.CgroupQuotaState{
				RefillRateTokensPerSec: spec.RefillRate,
				MaxBurstTokens:         spec.MaxBurst,
				CurrentTokens:          spec.MaxBurst,
				MaxVramBytes:           spec.MaxVramBytes,
				VramPolicy:             spec.VramPolicy,
				LastUpdateNs:           0,
			}
			if err := w.quotaMap.Put(&inode, &state); err != nil {
				w.mu.Unlock()
				log.Printf("cgroup watcher: failed to enroll cgroup %s (inode=%d) in quotaMap: %v", match, inode, err)
				continue
			}

			w.enrolled[inode] = true
			w.mu.Unlock()

			log.Printf("Enrolled cgroup path=%s (inode=%d): refill_rate=%d tokens/sec, max_burst=%d",
				match, inode, spec.RefillRate, spec.MaxBurst)
		}
	}
}

// Start runs SyncOnce immediately, then periodically checks at the given interval until ctx is cancelled.
func (w *Watcher) Start(ctx context.Context, interval time.Duration) {
	w.SyncOnce()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.SyncOnce()
		}
	}
}
