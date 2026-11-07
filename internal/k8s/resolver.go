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
	"github.com/motilayo/gpu-gatekeeper/internal/dra"
)

// WorkloadMeta holds Kubernetes workload identity for a cgroup.
type WorkloadMeta struct {
	Namespace     string
	Pod           string
	Container     string
	ResourceClaim string
}

type podList struct {
	Items []podItem `json:"items"`
}

type podItem struct {
	Metadata podMetadata `json:"metadata"`
	Spec     podSpec     `json:"spec"`
	Status   podStatus   `json:"status"`
}

type podMetadata struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	UID         string            `json:"uid"`
	Annotations map[string]string `json:"annotations"`
}

type podSpec struct {
	ResourceClaims []podResourceClaim `json:"resourceClaims"`
}

type podResourceClaim struct {
	Name                      string `json:"name"`
	ResourceClaimName         string `json:"resourceClaimName"`
	ResourceClaimTemplateName string `json:"resourceClaimTemplateName"`
}

type podStatus struct {
	ContainerStatuses     []containerStatus       `json:"containerStatuses"`
	ResourceClaimStatuses []podResourceClaimStatus `json:"resourceClaimStatuses"`
}

type podResourceClaimStatus struct {
	Name              string `json:"name"`
	ResourceClaimName string `json:"resourceClaimName"`
}

type containerStatus struct {
	Name        string `json:"name"`
	ContainerID string `json:"containerID"`
}

// Kubernetes DRA resource.k8s.io/v1 and resource.k8s.io/v1beta1 ResourceClaim structures
type resourceClaimList struct {
	Items []resourceClaimItem `json:"items"`
}

type resourceClaimItem struct {
	Metadata podMetadata         `json:"metadata"`
	Spec     resourceClaimSpec   `json:"spec"`
	Status   resourceClaimStatus `json:"status"`
}

type resourceClaimSpec struct {
	Devices deviceClaim `json:"devices"`
}

type deviceClaim struct {
	Config []deviceClaimConfiguration `json:"config"`
}

type deviceClaimConfiguration struct {
	Opaque *opaqueDeviceConfiguration `json:"opaque"`
}

type opaqueDeviceConfiguration struct {
	Driver     string                 `json:"driver"`
	Parameters map[string]interface{} `json:"parameters"`
}

type resourceClaimStatus struct {
	Allocation *allocationResult `json:"allocation"`
}

type allocationResult struct {
	Devices deviceAllocationResult `json:"devices"`
}

type deviceAllocationResult struct {
	Results []deviceRequestAllocationResult `json:"results"`
	Config  []deviceAllocationConfiguration `json:"config"`
}

type deviceRequestAllocationResult struct {
	Request          string            `json:"request"`
	Driver           string            `json:"driver"`
	Pool             string            `json:"pool"`
	Device           string            `json:"device"`
	ConsumedCapacity map[string]string `json:"consumedCapacity"`
}

type deviceAllocationConfiguration struct {
	Opaque *opaqueDeviceConfiguration `json:"opaque"`
}

type enrolledPolicy struct {
	refillRate   uint64
	maxBurst     uint64
	maxVramBytes uint64
}

// parseK8sQuantity parses standard Kubernetes quantity strings (Ki, Mi, Gi, Ti, k, M, G, T, or plain integers).
func parseK8sQuantity(val string) (uint64, bool) {
	val = strings.TrimSpace(val)
	if val == "" {
		return 0, false
	}
	// Binary suffixes
	if strings.HasSuffix(val, "Ki") {
		base, err := strconv.ParseUint(strings.TrimSuffix(val, "Ki"), 10, 64)
		return base * 1024, err == nil
	}
	if strings.HasSuffix(val, "Mi") {
		base, err := strconv.ParseUint(strings.TrimSuffix(val, "Mi"), 10, 64)
		return base * 1024 * 1024, err == nil
	}
	if strings.HasSuffix(val, "Gi") {
		base, err := strconv.ParseUint(strings.TrimSuffix(val, "Gi"), 10, 64)
		return base * 1024 * 1024 * 1024, err == nil
	}
	if strings.HasSuffix(val, "Ti") {
		base, err := strconv.ParseUint(strings.TrimSuffix(val, "Ti"), 10, 64)
		return base * 1024 * 1024 * 1024 * 1024, err == nil
	}
	// Decimal suffixes
	if strings.HasSuffix(val, "k") {
		base, err := strconv.ParseUint(strings.TrimSuffix(val, "k"), 10, 64)
		return base * 1000, err == nil
	}
	if strings.HasSuffix(val, "M") {
		base, err := strconv.ParseUint(strings.TrimSuffix(val, "M"), 10, 64)
		return base * 1000000, err == nil
	}
	if strings.HasSuffix(val, "G") {
		base, err := strconv.ParseUint(strings.TrimSuffix(val, "G"), 10, 64)
		return base * 1000000000, err == nil
	}
	if strings.HasSuffix(val, "T") {
		base, err := strconv.ParseUint(strings.TrimSuffix(val, "T"), 10, 64)
		return base * 1000000000000, err == nil
	}
	// Plain integer
	base, err := strconv.ParseUint(val, 10, 64)
	return base, err == nil
}

func parseUintFromInterface(v interface{}) (uint64, bool) {
	switch val := v.(type) {
	case float64:
		return uint64(val), true
	case int:
		return uint64(val), true
	case int64:
		return uint64(val), true
	case uint64:
		return val, true
	case string:
		u, err := strconv.ParseUint(val, 10, 64)
		return u, err == nil
	default:
		return 0, false
	}
}

// Resolver periodically discovers cgroup v2 paths, correlates them with Kubernetes
// Pod metadata from Kubelet and DRA ResourceClaims, and applies quotas automatically.
type Resolver struct {
	quotaMap   *ebpf.Map
	podsURL    string
	podsFile   string
	claimsURL  string
	claimsFile string
	tokenFile  string
	cgroupRoot string
	httpClient *http.Client
	draDriver  *dra.Driver

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

// SetDRAClaimsSource configures optional Kubernetes DRA resource.k8s.io ResourceClaim discovery.
func (r *Resolver) SetDRAClaimsSource(claimsURL, claimsFile string) {
	r.claimsURL = claimsURL
	r.claimsFile = claimsFile
}

// SetDRADriver registers the DRA driver for automated CDI spec generation and in-memory claim cache access.
func (r *Resolver) SetDRADriver(d *dra.Driver) {
	r.draDriver = d
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

func (r *Resolver) fetchResourceClaims() (map[string]resourceClaimItem, error) {
	var body []byte
	if r.claimsFile != "" {
		data, err := os.ReadFile(r.claimsFile)
		if err != nil {
			return nil, err
		}
		body = data
	} else if r.claimsURL != "" {
		req, err := http.NewRequest("GET", r.claimsURL, nil)
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

		if resp.StatusCode == http.StatusNotFound {
			// Fallback: Kubernetes 1.34 / 1.31 DRA API transition from resource.k8s.io/v1 to v1beta1
			fallbackURL := ""
			if strings.Contains(r.claimsURL, "/apis/resource.k8s.io/v1/") {
				fallbackURL = strings.Replace(r.claimsURL, "/apis/resource.k8s.io/v1/", "/apis/resource.k8s.io/v1beta1/", 1)
			} else if strings.Contains(r.claimsURL, "/resource.k8s.io/v1/") {
				fallbackURL = strings.Replace(r.claimsURL, "/resource.k8s.io/v1/", "/resource.k8s.io/v1beta1/", 1)
			} else if strings.Contains(r.claimsURL, "/v1/resourceclaims") {
				fallbackURL = strings.Replace(r.claimsURL, "/v1/resourceclaims", "/v1beta1/resourceclaims", 1)
			}

			if fallbackURL != "" {
				reqFallback, errFB := http.NewRequest("GET", fallbackURL, nil)
				if errFB == nil {
					if r.tokenFile != "" {
						if tokenBytes, err := os.ReadFile(r.tokenFile); err == nil && len(tokenBytes) > 0 {
							token := strings.TrimSpace(string(tokenBytes))
							if token != "" {
								reqFallback.Header.Set("Authorization", "Bearer "+token)
							}
						}
					}
					respFB, errFB := r.httpClient.Do(reqFallback)
					if errFB == nil {
						defer respFB.Body.Close()
						if respFB.StatusCode == http.StatusOK {
							dataFB, errFB := io.ReadAll(respFB.Body)
							if errFB == nil {
								body = dataFB
							}
						}
					}
				}
			}
			if body == nil {
				return nil, fmt.Errorf("DRA claims server returned HTTP 404 (and fallback v1beta1 failed)")
			}
		} else if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("DRA claims server returned HTTP %d", resp.StatusCode)
		} else {
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				return nil, err
			}
			body = data
		}
	} else {
		return nil, nil
	}

	var cl resourceClaimList
	if err := json.Unmarshal(body, &cl); err != nil {
		return nil, err
	}
	m := make(map[string]resourceClaimItem, len(cl.Items))
	for _, item := range cl.Items {
		key := item.Metadata.Namespace + "/" + item.Metadata.Name
		m[key] = item
		if item.Metadata.Namespace == "" || m[item.Metadata.Name].Metadata.Name == "" {
			m[item.Metadata.Name] = item
		}
	}
	return m, nil
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

	claims, err := r.fetchResourceClaims()
	if err != nil && (r.claimsFile != "" || r.claimsURL != "") {
		log.Printf("k8s resolver: failed to fetch DRA resource claims: %v", err)
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

				var refillRate uint64
				var maxBurst uint64
				var maxVramBytes uint64
				var boundClaimName string
				var boundClaimUID string

				// 1. Check if the Pod has any bound Kubernetes DRA ResourceClaim via claims API/file
				var boundClaim *resourceClaimItem
				for _, rcs := range pod.Status.ResourceClaimStatuses {
					if rcs.ResourceClaimName != "" {
						if c, ok := claims[pod.Metadata.Namespace+"/"+rcs.ResourceClaimName]; ok {
							boundClaim = &c
							boundClaimName = c.Metadata.Name
							boundClaimUID = c.Metadata.UID
							break
						} else if c, ok := claims[rcs.ResourceClaimName]; ok {
							boundClaim = &c
							boundClaimName = c.Metadata.Name
							boundClaimUID = c.Metadata.UID
							break
						}
					}
				}
				if boundClaim == nil {
					for _, rc := range pod.Spec.ResourceClaims {
						if rc.ResourceClaimName != "" {
							if c, ok := claims[pod.Metadata.Namespace+"/"+rc.ResourceClaimName]; ok {
								boundClaim = &c
								boundClaimName = c.Metadata.Name
								boundClaimUID = c.Metadata.UID
								break
							} else if c, ok := claims[rc.ResourceClaimName]; ok {
								boundClaim = &c
								boundClaimName = c.Metadata.Name
								boundClaimUID = c.Metadata.UID
								break
							}
						}
					}
				}

				// If not found in polled claims, check if it was pre-cached in Driver (Guardrail 1: NodePrepareResource ran before cgroup v2 created or before API poll)
				if boundClaim == nil && r.draDriver != nil {
					for _, rcs := range pod.Status.ResourceClaimStatuses {
						if rcs.ResourceClaimName != "" {
							if prep, ok := r.draDriver.GetPreparedClaimByName(pod.Metadata.Namespace, rcs.ResourceClaimName); ok {
								boundClaimName = prep.ClaimName
								boundClaimUID = prep.ClaimUID
								refillRate = prep.TokensPerSec
								maxBurst = prep.BurstTokens
								maxVramBytes = prep.MaxVramBytes
								break
							} else if prep, ok := r.draDriver.GetPreparedClaim(rcs.ResourceClaimName); ok {
								boundClaimName = prep.ClaimName
								boundClaimUID = prep.ClaimUID
								refillRate = prep.TokensPerSec
								maxBurst = prep.BurstTokens
								maxVramBytes = prep.MaxVramBytes
								break
							}
						}
					}
					if boundClaimName == "" {
						for _, rc := range pod.Spec.ResourceClaims {
							if rc.ResourceClaimName != "" {
								if prep, ok := r.draDriver.GetPreparedClaimByName(pod.Metadata.Namespace, rc.ResourceClaimName); ok {
									boundClaimName = prep.ClaimName
									boundClaimUID = prep.ClaimUID
									refillRate = prep.TokensPerSec
									maxBurst = prep.BurstTokens
									maxVramBytes = prep.MaxVramBytes
									break
								} else if prep, ok := r.draDriver.GetPreparedClaim(rc.ResourceClaimName); ok {
									boundClaimName = prep.ClaimName
									boundClaimUID = prep.ClaimUID
									refillRate = prep.TokensPerSec
									maxBurst = prep.BurstTokens
									maxVramBytes = prep.MaxVramBytes
									break
								}
							}
						}
					}
				}

				newInodeToMeta[inode] = WorkloadMeta{
					Namespace:     pod.Metadata.Namespace,
					Pod:           pod.Metadata.Name,
					Container:     containerName,
					ResourceClaim: boundClaimName,
				}

				if boundClaim != nil {
					// Extract from claim annotations
					if boundClaim.Metadata.Annotations != nil {
						if rStr, ok := boundClaim.Metadata.Annotations["g-gate.io/tokens-per-sec"]; ok {
							if r, err := strconv.ParseUint(rStr, 10, 64); err == nil && r > 0 {
								refillRate = r
							}
						}
						if bStr, ok := boundClaim.Metadata.Annotations["g-gate.io/burst-tokens"]; ok {
							if b, err := strconv.ParseUint(bStr, 10, 64); err == nil && b > 0 {
								maxBurst = b
							}
						}
						if vStr, ok := boundClaim.Metadata.Annotations["g-gate.io/max-vram-mb"]; ok {
							if mb, err := strconv.ParseUint(vStr, 10, 64); err == nil && mb > 0 {
								maxVramBytes = mb * 1024 * 1024
							}
						}
					}

					// Extract from spec.devices.config[].opaque.parameters
					for _, cfg := range boundClaim.Spec.Devices.Config {
						if cfg.Opaque != nil && cfg.Opaque.Parameters != nil {
							if v, ok := cfg.Opaque.Parameters["tokensPerSec"]; ok {
								if u, ok := parseUintFromInterface(v); ok && u > 0 {
									refillRate = u
								}
							}
							if v, ok := cfg.Opaque.Parameters["burstTokens"]; ok {
								if u, ok := parseUintFromInterface(v); ok && u > 0 {
									maxBurst = u
								}
							}
							if v, ok := cfg.Opaque.Parameters["maxVramMB"]; ok {
								if u, ok := parseUintFromInterface(v); ok && u > 0 {
									maxVramBytes = u * 1024 * 1024
								}
							}
						}
					}

					// Extract from status.allocation
					if boundClaim.Status.Allocation != nil {
						// status.allocation.devices.config[].opaque.parameters
						for _, cfg := range boundClaim.Status.Allocation.Devices.Config {
							if cfg.Opaque != nil && cfg.Opaque.Parameters != nil {
								if v, ok := cfg.Opaque.Parameters["tokensPerSec"]; ok {
									if u, ok := parseUintFromInterface(v); ok && u > 0 {
										refillRate = u
									}
								}
								if v, ok := cfg.Opaque.Parameters["burstTokens"]; ok {
									if u, ok := parseUintFromInterface(v); ok && u > 0 {
										maxBurst = u
									}
								}
								if v, ok := cfg.Opaque.Parameters["maxVramMB"]; ok {
									if u, ok := parseUintFromInterface(v); ok && u > 0 {
										maxVramBytes = u * 1024 * 1024
									}
								}
							}
						}

						// status.allocation.devices.results[].consumedCapacity
						for _, res := range boundClaim.Status.Allocation.Devices.Results {
							if res.ConsumedCapacity != nil {
								// tokens-per-sec or gpu.g-gate.io/tokens-per-sec
								if tStr, ok := res.ConsumedCapacity["gpu.g-gate.io/tokens-per-sec"]; ok {
									if tRate, err := strconv.ParseUint(tStr, 10, 64); err == nil && tRate > 0 {
										refillRate = tRate
									}
								} else if tStr, ok := res.ConsumedCapacity["tokens-per-sec"]; ok {
									if tRate, err := strconv.ParseUint(tStr, 10, 64); err == nil && tRate > 0 {
										refillRate = tRate
									}
								}

								// shares or gpu.g-gate.io/shares (if refillRate == 0, shares * 320)
								if refillRate == 0 {
									if sStr, ok := res.ConsumedCapacity["gpu.g-gate.io/shares"]; ok {
										if shares, err := strconv.ParseUint(sStr, 10, 64); err == nil && shares > 0 {
											refillRate = shares * 320
										}
									} else if sStr, ok := res.ConsumedCapacity["shares"]; ok {
										if shares, err := strconv.ParseUint(sStr, 10, 64); err == nil && shares > 0 {
											refillRate = shares * 320
										}
									}
								}

								// burst-tokens or gpu.g-gate.io/burst-tokens
								if bStr, ok := res.ConsumedCapacity["gpu.g-gate.io/burst-tokens"]; ok {
									if burst, err := strconv.ParseUint(bStr, 10, 64); err == nil && burst > 0 {
										maxBurst = burst
									}
								} else if bStr, ok := res.ConsumedCapacity["burst-tokens"]; ok {
									if burst, err := strconv.ParseUint(bStr, 10, 64); err == nil && burst > 0 {
										maxBurst = burst
									}
								}

								// memory or max-vram or gpu.g-gate.io/max-vram
								for _, memKey := range []string{"gpu.g-gate.io/max-vram", "max-vram", "memory"} {
									if memStr, ok := res.ConsumedCapacity[memKey]; ok {
										if bytes, ok := parseK8sQuantity(memStr); ok && bytes > 0 {
											maxVramBytes = bytes
											break
										}
									}
								}
							}
						}
					}
				}

				// 2. Check Pod annotations (allows override or fallback when no ResourceClaim is bound)
				if pod.Metadata.Annotations != nil {
					if rateStr, ok := pod.Metadata.Annotations["g-gate.io/tokens-per-sec"]; ok {
						if rRate, err := strconv.ParseUint(rateStr, 10, 64); err == nil && rRate > 0 {
							refillRate = rRate
						}
					}
					if burstStr, ok := pod.Metadata.Annotations["g-gate.io/burst-tokens"]; ok {
						if burst, err := strconv.ParseUint(burstStr, 10, 64); err == nil && burst > 0 {
							maxBurst = burst
						}
					}
					if vramStr, ok := pod.Metadata.Annotations["g-gate.io/max-vram-mb"]; ok {
						if mb, err := strconv.ParseUint(vramStr, 10, 64); err == nil && mb > 0 {
							maxVramBytes = mb * 1024 * 1024
						}
					}
				}

				// Default maxBurst fallback: if maxBurst == 0 && refillRate > 0
				if maxBurst == 0 && refillRate > 0 {
					if refillRate >= 100 {
						maxBurst = refillRate / 100
					} else {
						maxBurst = refillRate
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
							if boundClaimName != "" {
								log.Printf("k8s auto-enrolled cgroup %s (inode=%d) from DRA ResourceClaim %s (pod %s/%s): refill_rate=%d, burst=%d, max_vram_bytes=%d",
									path, inode, boundClaimName, pod.Metadata.Namespace, pod.Metadata.Name, refillRate, maxBurst, maxVramBytes)
							} else {
								log.Printf("k8s auto-enrolled cgroup %s (inode=%d) from pod %s/%s annotation: refill_rate=%d, burst=%d, max_vram_bytes=%d",
									path, inode, pod.Metadata.Namespace, pod.Metadata.Name, refillRate, maxBurst, maxVramBytes)
							}
						} else {
							log.Printf("k8s failed to enroll cgroup %s (inode=%d): %v", path, inode, err)
						}

						// Automatically prepare CDI spec if DRA driver is configured
						if r.draDriver != nil && boundClaimName != "" {
							uid := boundClaimUID
							if uid == "" {
								uid = boundClaimName
							}
							_, _ = r.draDriver.NodePrepareResource(dra.PreparedClaim{
								ClaimUID:     uid,
								ClaimName:    boundClaimName,
								Namespace:    pod.Metadata.Namespace,
								DeviceID:     "gpu-0",
								TokensPerSec: refillRate,
								BurstTokens:  maxBurst,
								MaxVramBytes: maxVramBytes,
							})
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
				Namespace:     "host",
				Pod:           base,
				Container:     "default",
				ResourceClaim: "",
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
			Namespace:     "host",
			Pod:           base,
			Container:     "default",
			ResourceClaim: "",
		}
	}

	return WorkloadMeta{
		Namespace:     "unknown",
		Pod:           "unknown",
		Container:     "unknown",
		ResourceClaim: "",
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
