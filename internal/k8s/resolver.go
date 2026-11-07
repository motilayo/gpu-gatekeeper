package k8s

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/motilayo/gpu-gatekeeper/internal/bpf"
)

// WorkloadMeta holds Kubernetes workload identity for a cgroup.
type WorkloadMeta struct {
	Namespace string
	Pod       string
	Container string
}

type podList struct {
	Items []podItem `json:"items"`
}

type podItem struct {
	Metadata podMetadata `json:"metadata"`
	Status   podStatus   `json:"status"`
}

type podMetadata struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	UID         string            `json:"uid"`
	Annotations map[string]string `json:"annotations"`
}

type podStatus struct {
	ContainerStatuses []containerStatus `json:"containerStatuses"`
}

type containerStatus struct {
	Name        string `json:"name"`
	ContainerID string `json:"containerID"`
}

type enrolledPolicy struct {
	refillRate   uint64
	maxBurst     uint64
	maxVramBytes uint64
}

// Resolver periodically discovers cgroup v2 paths, correlates them with Kubernetes
// Pod metadata from Kubelet, and applies Pod annotation quotas automatically.
type Resolver struct {
	quotaMap   *ebpf.Map
	podsURL    string
	podsFile   string
	tokenFile  string
	cgroupRoot string
	httpClient *http.Client

	mu          sync.RWMutex
	inodeToPath map[uint64]string
	inodeToMeta map[uint64]WorkloadMeta
	enrolled    map[uint64]enrolledPolicy
}

// NewResolver initializes a new Kubernetes Pod and cgroup metadata resolver.
func NewResolver(quotaMap *ebpf.Map, podsURL, podsFile, tokenFile string) *Resolver {
	return &Resolver{
		quotaMap:   quotaMap,
		podsURL:    podsURL,
		podsFile:   podsFile,
		tokenFile:  tokenFile,
		cgroupRoot: "/sys/fs/cgroup",
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
		inodeToPath: make(map[uint64]string),
		inodeToMeta: make(map[uint64]WorkloadMeta),
		enrolled:    make(map[uint64]enrolledPolicy),
	}
}

func (r *Resolver) fetchPods() ([]podItem, error) {
	var body []byte
	if r.podsFile != "" {
		data, err := os.ReadFile(r.podsFile)
		if err != nil {
			return nil, err
		}
		body = data
	} else if r.podsURL != "" {
		req, err := http.NewRequest("GET", r.podsURL, nil)
		if err != nil {
			return nil, err
		}
		if r.tokenFile != "" {
			if tokenBytes, err := os.ReadFile(r.tokenFile); err == nil && len(tokenBytes) > 0 {
				token := strings.TrimSpace(string(tokenBytes))
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
			}
		}
		resp, err := r.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("kubelet returned HTTP %d", resp.StatusCode)
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		body = data
	} else {
		return nil, nil
	}

	var pl podList
	if err := json.Unmarshal(body, &pl); err != nil {
		return nil, err
	}
	return pl.Items, nil
}

// SyncOnce scans /sys/fs/cgroup, resolves pod metadata, and enrolls annotated quotas.
func (r *Resolver) SyncOnce() {
	newInodeToPath := make(map[uint64]string)

	_ = filepath.WalkDir(r.cgroupRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		newInodeToPath[uint64(stat.Ino)] = path
		return nil
	})

	pods, err := r.fetchPods()
	if err != nil && (r.podsFile != "" || r.podsURL != "") {
		log.Printf("k8s resolver: failed to fetch pods: %v", err)
	}

	newInodeToMeta := make(map[uint64]WorkloadMeta)

	for inode, path := range newInodeToPath {
		matched := false
		for _, pod := range pods {
			uidDashed := pod.Metadata.UID
			uidUnderscore := strings.ReplaceAll(uidDashed, "-", "_")

			if (uidDashed != "" && strings.Contains(path, uidDashed)) ||
				(uidUnderscore != "" && strings.Contains(path, uidUnderscore)) {
				matched = true

				containerName := ""
				for _, cs := range pod.Status.ContainerStatuses {
					rawID := cs.ContainerID
					if idx := strings.Index(rawID, "://"); idx != -1 {
						rawID = rawID[idx+3:]
					}
					if rawID != "" && strings.Contains(path, rawID) {
						containerName = cs.Name
						break
					}
				}
				if containerName == "" && len(pod.Status.ContainerStatuses) > 0 {
					containerName = pod.Status.ContainerStatuses[0].Name
				}
				if containerName == "" {
					containerName = "pod"
				}

				newInodeToMeta[inode] = WorkloadMeta{
					Namespace: pod.Metadata.Namespace,
					Pod:       pod.Metadata.Name,
					Container: containerName,
				}

				// Check Pod annotations for compute token-bucket and VRAM quotas
				rateStr, hasRate := pod.Metadata.Annotations["g-gate.io/tokens-per-sec"]
				vramStr, hasVram := pod.Metadata.Annotations["g-gate.io/max-vram-mb"]

				var refillRate uint64
				var maxBurst uint64
				if hasRate {
					if rRate, err := strconv.ParseUint(rateStr, 10, 64); err == nil && rRate > 0 {
						refillRate = rRate
						maxBurst = rRate
						if burstStr, ok := pod.Metadata.Annotations["g-gate.io/burst-tokens"]; ok {
							if burst, err := strconv.ParseUint(burstStr, 10, 64); err == nil && burst > 0 {
								maxBurst = burst
							}
						}
					}
				}

				var maxVramBytes uint64
				if hasVram {
					if mb, err := strconv.ParseUint(vramStr, 10, 64); err == nil && mb > 0 {
						maxVramBytes = mb * 1024 * 1024
					}
				}

				if refillRate > 0 || maxVramBytes > 0 {
					newPol := enrolledPolicy{
						refillRate:   refillRate,
						maxBurst:     maxBurst,
						maxVramBytes: maxVramBytes,
					}

					r.mu.Lock()
					prevPol, alreadyEnrolled := r.enrolled[inode]
					if !alreadyEnrolled || prevPol != newPol {
						var existingState bpf.CgroupQuotaState
						if r.quotaMap != nil {
							_ = r.quotaMap.Lookup(&inode, &existingState)
						}

						existingState.RefillRateTokensPerSec = refillRate
						existingState.MaxBurstTokens = maxBurst
						if existingState.CurrentTokens == 0 && maxBurst > 0 {
							existingState.CurrentTokens = maxBurst
						}
						existingState.MaxVramBytes = maxVramBytes
						if maxVramBytes > 0 && existingState.AllocatedVramBytes > maxVramBytes && existingState.VramQuotaExceeded == 0 {
							existingState.VramQuotaExceeded = 1
						}

						if r.quotaMap == nil {
							r.enrolled[inode] = newPol
						} else if err := r.quotaMap.Put(&inode, &existingState); err == nil {
							r.enrolled[inode] = newPol
							log.Printf("k8s auto-enrolled cgroup %s (inode=%d) from pod %s/%s annotation: refill_rate=%d, burst=%d, max_vram_bytes=%d",
								path, inode, pod.Metadata.Namespace, pod.Metadata.Name, refillRate, maxBurst, maxVramBytes)
						} else {
							log.Printf("k8s failed to enroll cgroup %s (inode=%d): %v", path, inode, err)
						}
					}
					r.mu.Unlock()
				}
				break
			}
		}

		if !matched {
			base := filepath.Base(path)
			if base == "/" || base == "." || base == "cgroup" {
				base = "root"
			}
			newInodeToMeta[inode] = WorkloadMeta{
				Namespace: "host",
				Pod:       base,
				Container: "default",
			}
		}
	}

	r.mu.Lock()
	for k, v := range newInodeToPath {
		r.inodeToPath[k] = v
	}
	for k, v := range newInodeToMeta {
		if prev, exists := r.inodeToMeta[k]; !exists || (prev.Namespace == "host" || prev.Namespace == "unknown") || (v.Namespace != "host" && v.Namespace != "unknown") {
			r.inodeToMeta[k] = v
		}
	}
	r.mu.Unlock()
}

// LookupMeta returns the resolved Kubernetes workload metadata for a cgroup inode.
func (r *Resolver) LookupMeta(inode uint64) WorkloadMeta {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if meta, ok := r.inodeToMeta[inode]; ok {
		return meta
	}

	if path, ok := r.inodeToPath[inode]; ok {
		base := filepath.Base(path)
		if base == "/" || base == "." || base == "cgroup" {
			base = "root"
		}
		return WorkloadMeta{
			Namespace: "host",
			Pod:       base,
			Container: "default",
		}
	}

	return WorkloadMeta{
		Namespace: "unknown",
		Pod:       "unknown",
		Container: "unknown",
	}
}

// Start begins periodic background reconciliation.
func (r *Resolver) Start(ctx context.Context, interval time.Duration) {
	r.SyncOnce()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.SyncOnce()
		}
	}
}
