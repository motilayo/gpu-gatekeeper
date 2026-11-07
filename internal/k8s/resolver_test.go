package k8s

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestResolverHTTPSBearerTokenAndPodAttribution(t *testing.T) {
	tmpCgroupRoot := t.TempDir()
	leafPath := filepath.Join(
		tmpCgroupRoot,
		"kubepods.slice",
		"kubepods-burstable.slice",
		"kubepods-burstable-pod11112222_3333_4444_5555_666677778888.slice",
		"cri-containerd-abcdef0123456789.scope",
	)
	if err := os.MkdirAll(leafPath, 0755); err != nil {
		t.Fatalf("mkdir leaf path: %v", err)
	}

	info, err := os.Stat(leafPath)
	if err != nil {
		t.Fatalf("stat leaf path: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("could not extract syscall.Stat_t")
	}
	inode := uint64(stat.Ino)

	tokenFile := filepath.Join(t.TempDir(), "sa.token")
	if err := os.WriteFile(tokenFile, []byte("my-secret-k3s-token\n"), 0600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	mockPods := podList{
		Items: []podItem{
			{
				Metadata: podMetadata{
					Name:      "vllm-llama3",
					Namespace: "ai-prod",
					UID:       "11112222-3333-4444-5555-666677778888",
					Annotations: map[string]string{
						"g-gate.io/tokens-per-sec": "250",
						"g-gate.io/burst-tokens":    "50",
						"g-gate.io/max-vram-mb":     "128",
					},
				},
				Status: podStatus{
					ContainerStatuses: []containerStatus{
						{
							Name:        "vllm",
							ContainerID: "containerd://abcdef0123456789",
						},
					},
				},
			},
		},
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer my-secret-k3s-token" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mockPods)
	}))
	defer srv.Close()

	res := NewResolver(nil, srv.URL+"/pods", "", tokenFile)
	res.cgroupRoot = tmpCgroupRoot
	res.SyncOnce()

	expectedMeta := WorkloadMeta{
		Namespace: "ai-prod",
		Pod:       "vllm-llama3",
		Container: "vllm",
	}
	actualMeta := res.LookupMeta(inode)
	if actualMeta != expectedMeta {
		t.Fatalf("LookupMeta mismatch: got %+v, want %+v", actualMeta, expectedMeta)
	}

	res.mu.RLock()
	actualPol, enrolled := res.enrolled[inode]
	res.mu.RUnlock()

	if !enrolled {
		t.Fatalf("expected inode %d to be enrolled in policies", inode)
	}
	expectedPol := enrolledPolicy{
		refillRate:   250,
		maxBurst:     50,
		maxVramBytes: 128 * 1024 * 1024,
	}
	if actualPol != expectedPol {
		t.Fatalf("enrolledPolicy mismatch: got %+v, want %+v", actualPol, expectedPol)
	}
}

func TestResolverDRAResourceClaimEnforcement(t *testing.T) {
	tmpCgroupRoot := t.TempDir()
	leafPath := filepath.Join(
		tmpCgroupRoot,
		"kubepods.slice",
		"kubepods-burstable.slice",
		"kubepods-burstable-pod22223333_4444_5555_6666_777788889999.slice",
		"cri-containerd-abcdef1234567890.scope",
	)
	if err := os.MkdirAll(leafPath, 0755); err != nil {
		t.Fatalf("mkdir leaf path: %v", err)
	}

	info, err := os.Stat(leafPath)
	if err != nil {
		t.Fatalf("stat leaf path: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("could not extract syscall.Stat_t")
	}
	inode := uint64(stat.Ino)

	tokenFile := filepath.Join(t.TempDir(), "sa.token")
	if err := os.WriteFile(tokenFile, []byte("dra-secret-token\n"), 0600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	// Pod with ZERO annotations, referencing ResourceClaim via status.resourceClaimStatuses
	mockPods := podList{
		Items: []podItem{
			{
				Metadata: podMetadata{
					Name:        "llama3-dra-pod",
					Namespace:   "ai-prod",
					UID:         "22223333-4444-5555-6666-777788889999",
					Annotations: nil, // Zero g-gate annotations!
				},
				Status: podStatus{
					ContainerStatuses: []containerStatus{
						{
							Name:        "llama3",
							ContainerID: "containerd://abcdef1234567890",
						},
					},
					ResourceClaimStatuses: []podResourceClaimStatus{
						{
							Name:              "gpu",
							ResourceClaimName: "llama3-gpu-claim",
						},
					},
				},
			},
		},
	}

	mockClaims := resourceClaimList{
		Items: []resourceClaimItem{
			{
				Metadata: podMetadata{
					Name:      "llama3-gpu-claim",
					Namespace: "ai-prod",
					UID:       "claim-uid-12345",
				},
				Status: resourceClaimStatus{
					Allocation: &allocationResult{
						Devices: deviceAllocationResult{
							Results: []deviceRequestAllocationResult{
								{
									Request: "gpu",
									Driver:  "gpu.g-gate.io",
									ConsumedCapacity: map[string]string{
										"gpu.g-gate.io/tokens-per-sec": "6400",
										"gpu.g-gate.io/burst-tokens":   "64",
										"memory":                       "32Mi",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer dra-secret-token" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/pods":
			_ = json.NewEncoder(w).Encode(mockPods)
		case "/claims":
			_ = json.NewEncoder(w).Encode(mockClaims)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	res := NewResolver(nil, srv.URL+"/pods", "", tokenFile)
	res.SetDRAClaimsSource(srv.URL+"/claims", "")
	res.cgroupRoot = tmpCgroupRoot
	res.SyncOnce()

	expectedMeta := WorkloadMeta{
		Namespace: "ai-prod",
		Pod:       "llama3-dra-pod",
		Container: "llama3",
	}
	actualMeta := res.LookupMeta(inode)
	if actualMeta != expectedMeta {
		t.Fatalf("LookupMeta mismatch: got %+v, want %+v", actualMeta, expectedMeta)
	}

	res.mu.RLock()
	actualPol, enrolled := res.enrolled[inode]
	res.mu.RUnlock()

	if !enrolled {
		t.Fatalf("expected inode %d to be enrolled in policies via DRA ResourceClaim", inode)
	}
	expectedPol := enrolledPolicy{
		refillRate:   6400,
		maxBurst:     64,
		maxVramBytes: 32 * 1024 * 1024, // 33554432 bytes
	}
	if actualPol != expectedPol {
		t.Fatalf("enrolledPolicy mismatch: got %+v, want %+v", actualPol, expectedPol)
	}
}
