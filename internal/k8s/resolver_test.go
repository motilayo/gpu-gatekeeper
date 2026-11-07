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
