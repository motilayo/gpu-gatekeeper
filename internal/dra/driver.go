package dra

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// DriverName is the canonical Kubernetes DRA driver domain.
	DriverName = "gpu.g-gate.io"
	// CDIVersion is the Container Device Interface specification version.
	CDIVersion = "0.6.0"
	// CDIKind is the qualified kind for G-Gate GPU devices.
	CDIKind = "gpu.g-gate.io/gpu"
)

// PreparedClaim describes a DRA ResourceClaim prepared for container injection.
type PreparedClaim struct {
	ClaimUID     string `json:"claimUID"`
	ClaimName    string `json:"claimName"`
	Namespace    string `json:"namespace"`
	DeviceID     string `json:"deviceID"`
	CDIDeviceID  string `json:"cdiDeviceID"` // e.g. "gpu.g-gate.io/gpu=claim-<uid>"
	TokensPerSec uint64 `json:"tokensPerSec"`
	BurstTokens  uint64 `json:"burstTokens"`
	MaxVramBytes uint64 `json:"maxVramBytes"`
	VRAMPolicy   string `json:"vramPolicy,omitempty"`
}

// CDISpec represents the Container Device Interface 0.6.0 JSON specification.
type CDISpec struct {
	CDIVersion string      `json:"cdiVersion"`
	Kind       string      `json:"kind"` // "gpu.g-gate.io/gpu"
	Devices    []CDIDevice `json:"devices"`
}

// CDIDevice defines a single claim-backed device within the CDI specification.
type CDIDevice struct {
	Name           string           `json:"name"` // "claim-<uid>"
	ContainerEdits CDIContainerEdit `json:"containerEdits"`
}

// CDIContainerEdit configures environment variables and device nodes for the container.
type CDIContainerEdit struct {
	Env         []string        `json:"env,omitempty"`
	DeviceNodes []CDIDeviceNode `json:"deviceNodes,omitempty"`
}

// CDIDeviceNode defines a host-to-container device node mount.
type CDIDeviceNode struct {
	Path     string `json:"path"`
	HostPath string `json:"hostPath,omitempty"`
}

// ClaimPrepareItem represents a single claim in a NodePrepareResources request.
type ClaimPrepareItem struct {
	UID          string `json:"uid"`
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	DeviceID     string `json:"deviceID,omitempty"`
	TokensPerSec uint64 `json:"tokensPerSec"`
	BurstTokens  uint64 `json:"burstTokens"`
	MaxVramBytes uint64 `json:"maxVramBytes"`
	VRAMPolicy   string `json:"vramPolicy,omitempty"`
}

// NodePrepareRequest represents the request body for /v1/NodePrepareResources.
type NodePrepareRequest struct {
	Claims []ClaimPrepareItem `json:"claims"`
}

// ClaimPrepareResult represents the preparation result for a single claim.
type ClaimPrepareResult struct {
	CDIDevices []string `json:"cdiDevices"`
}

// NodePrepareResponse represents the response body for /v1/NodePrepareResources.
type NodePrepareResponse struct {
	Claims map[string]ClaimPrepareResult `json:"claims"`
}

// NodeUnprepareRequest represents the request body for /v1/NodeUnprepareResources.
type NodeUnprepareRequest struct {
	ClaimUIDs []string `json:"claimUIDs"`
}

// NodeUnprepareResponse represents the response body for /v1/NodeUnprepareResources.
type NodeUnprepareResponse struct {
	Unprepared []string `json:"unprepared"`
}

// Driver manages DRA NodePrepareResource / NodeUnprepareResource operations
// and writes CDI 0.6.0 specification files to the host container runtime.
type Driver struct {
	cdiDir string
	mu     sync.RWMutex
	claims map[string]PreparedClaim
}

// NewDriver initializes a new DRA driver writing CDI specs to cdiDir.
func NewDriver(cdiDir string) *Driver {
	return &Driver{
		cdiDir: cdiDir,
		claims: make(map[string]PreparedClaim),
	}
}

// NodePrepareResource prepares a claim, registers it, and generates/updates the CDI spec.
func (d *Driver) NodePrepareResource(claim PreparedClaim) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	uid := claim.ClaimUID
	if uid == "" {
		uid = claim.ClaimName
	}
	claim.CDIDeviceID = fmt.Sprintf("%s/gpu=claim-%s", DriverName, uid)
	d.claims[uid] = claim

	if err := d.writeCDISpecLocked(); err != nil {
		return "", fmt.Errorf("write CDI spec: %w", err)
	}

	return claim.CDIDeviceID, nil
}

// NodeUnprepareResource removes a prepared claim and updates the CDI specification file.
func (d *Driver) NodeUnprepareResource(claimUID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	delete(d.claims, claimUID)
	if err := d.writeCDISpecLocked(); err != nil {
		return fmt.Errorf("write CDI spec after unprepare: %w", err)
	}
	return nil
}

// GetPreparedClaim returns a registered prepared claim by its UID.
func (d *Driver) GetPreparedClaim(claimUID string) (PreparedClaim, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	c, ok := d.claims[claimUID]
	return c, ok
}

// GetPreparedClaimByName returns a registered prepared claim by its namespace and claim name.
func (d *Driver) GetPreparedClaimByName(namespace, claimName string) (PreparedClaim, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	for _, c := range d.claims {
		if c.ClaimName == claimName && (namespace == "" || c.Namespace == "" || c.Namespace == namespace) {
			return c, true
		}
	}
	return PreparedClaim{}, false
}

// StartSocketServer starts a UNIX domain socket server listening for DRA RPC calls.
// It returns a cleanup function to gracefully shut down the server and remove the socket file.
func (d *Driver) StartSocketServer(sockPath string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(sockPath), 0755); err != nil {
		return nil, err
	}
	_ = os.Remove(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", sockPath, err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/v1/NodePrepareResources", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req NodePrepareRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}

		resp := NodePrepareResponse{
			Claims: make(map[string]ClaimPrepareResult),
		}

		for _, item := range req.Claims {
			uid := item.UID
			if uid == "" {
				uid = item.Name
			}
			claim := PreparedClaim{
				ClaimUID:     uid,
				ClaimName:    item.Name,
				Namespace:    item.Namespace,
				DeviceID:     item.DeviceID,
				TokensPerSec: item.TokensPerSec,
				BurstTokens:  item.BurstTokens,
				MaxVramBytes: item.MaxVramBytes,
				VRAMPolicy:   item.VRAMPolicy,
			}
			cdiDeviceID, err := d.NodePrepareResource(claim)
			if err != nil {
				http.Error(w, fmt.Sprintf("failed to prepare claim %s: %v", uid, err), http.StatusInternalServerError)
				return
			}
			resp.Claims[uid] = ClaimPrepareResult{
				CDIDevices: []string{cdiDeviceID},
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/v1/NodeUnprepareResources", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req NodeUnprepareRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}

		resp := NodeUnprepareResponse{
			Unprepared: make([]string, 0, len(req.ClaimUIDs)),
		}

		for _, uid := range req.ClaimUIDs {
			_ = d.NodeUnprepareResource(uid)
			resp.Unprepared = append(resp.Unprepared, uid)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	srv := &http.Server{
		Handler: mux,
	}

	go func() {
		_ = srv.Serve(listener)
	}()

	cleanup := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = listener.Close()
		_ = os.Remove(sockPath)
		return nil
	}

	return cleanup, nil
}

// BuildResourceSlice generates a Kubernetes 1.34 resource.k8s.io/v1 ResourceSlice object structure.
func BuildResourceSlice(nodeName string, capacityTokensPerSec uint64, capacityMemoryBytes uint64) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "resource.k8s.io/v1",
		"kind":       "ResourceSlice",
		"metadata": map[string]interface{}{
			"name": fmt.Sprintf("%s-gpu-g-gate-io", nodeName),
		},
		"spec": map[string]interface{}{
			"nodeName": nodeName,
			"driver":   DriverName,
			"pool": map[string]interface{}{
				"name":               nodeName,
				"generation":         1,
				"resourceSliceCount": 1,
			},
			"devices": []map[string]interface{}{
				{
					"name": "gpu-0",
					"basic": map[string]interface{}{
						"capacity": map[string]interface{}{
							"gpu.g-gate.io/tokens-per-sec": fmt.Sprintf("%d", capacityTokensPerSec),
							"gpu.g-gate.io/burst-tokens":  fmt.Sprintf("%d", capacityTokensPerSec),
							"memory":                      fmt.Sprintf("%dMi", capacityMemoryBytes/(1024*1024)),
						},
					},
				},
			},
		},
	}
}

// WriteResourceSliceFile serializes the ResourceSlice JSON to the specified file path.
func WriteResourceSliceFile(path, nodeName string, capacityTokensPerSec, capacityMemoryBytes uint64) error {
	slice := BuildResourceSlice(nodeName, capacityTokensPerSec, capacityMemoryBytes)
	data, err := json.MarshalIndent(slice, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// PublishResourceSliceHTTP publishes the ResourceSlice JSON to the Kubernetes API server endpoint via POST or PUT.
func PublishResourceSliceHTTP(apiURL, bearerToken, nodeName string, capacityTokensPerSec, capacityMemoryBytes uint64) error {
	slice := BuildResourceSlice(nodeName, capacityTokensPerSec, capacityMemoryBytes)
	data, err := json.Marshal(slice)
	if err != nil {
		return err
	}

	targetURL := apiURL
	if !strings.Contains(targetURL, "/apis/resource.k8s.io/v1/resourceslices") {
		targetURL = strings.TrimRight(targetURL, "/") + "/apis/resource.k8s.io/v1/resourceslices"
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 10 * time.Second,
	}

	req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		// Already exists; update via PUT
		sliceName := fmt.Sprintf("%s-gpu-g-gate-io", nodeName)
		putURL := fmt.Sprintf("%s/%s", strings.TrimRight(targetURL, "/"), sliceName)
		putReq, err := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(data))
		if err != nil {
			return err
		}
		putReq.Header.Set("Content-Type", "application/json")
		if bearerToken != "" {
			putReq.Header.Set("Authorization", "Bearer "+bearerToken)
		}
		putResp, err := client.Do(putReq)
		if err != nil {
			return err
		}
		defer putResp.Body.Close()
		if putResp.StatusCode >= 300 {
			body, _ := io.ReadAll(putResp.Body)
			return fmt.Errorf("PUT ResourceSlice returned %d: %s", putResp.StatusCode, string(body))
		}
		return nil
	}

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST ResourceSlice returned %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func (d *Driver) writeCDISpecLocked() error {
	if d.cdiDir == "" {
		return nil
	}
	if err := os.MkdirAll(d.cdiDir, 0755); err != nil {
		return err
	}

	spec := CDISpec{
		CDIVersion: CDIVersion,
		Kind:       CDIKind,
		Devices:    make([]CDIDevice, 0, len(d.claims)),
	}

	// Hardware-safe device nodes: ONLY check and include if /dev/nvidiactl is present on the host
	var commonNodes []CDIDeviceNode
	if _, err := os.Stat("/dev/nvidiactl"); err == nil {
		for _, dev := range []string{"/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools", "/dev/nvidia0"} {
			if _, sErr := os.Stat(dev); sErr == nil {
				commonNodes = append(commonNodes, CDIDeviceNode{
					Path:     dev,
					HostPath: dev,
				})
			}
		}
	}

	for uid, claim := range d.claims {
		env := []string{
			fmt.Sprintf("GGATE_CLAIM_UID=%s", uid),
			fmt.Sprintf("G_GATE_CLAIM_UID=%s", uid),
			fmt.Sprintf("G_GATE_TOKENS_PER_SEC=%d", claim.TokensPerSec),
			fmt.Sprintf("G_GATE_BURST_TOKENS=%d", claim.BurstTokens),
			fmt.Sprintf("G_GATE_MAX_VRAM_BYTES=%d", claim.MaxVramBytes),
		}
		if claim.ClaimName != "" {
			env = append(env,
				fmt.Sprintf("GGATE_RESOURCE_CLAIM=%s", claim.ClaimName),
				fmt.Sprintf("G_GATE_CLAIM_NAME=%s", claim.ClaimName),
			)
		}
		if claim.Namespace != "" {
			env = append(env, fmt.Sprintf("G_GATE_NAMESPACE=%s", claim.Namespace))
		}
		if claim.VRAMPolicy != "" {
			env = append(env,
				fmt.Sprintf("GGATE_VRAM_POLICY=%s", claim.VRAMPolicy),
				fmt.Sprintf("G_GATE_VRAM_POLICY=%s", claim.VRAMPolicy),
			)
		}

		dev := CDIDevice{
			Name: fmt.Sprintf("claim-%s", uid),
			ContainerEdits: CDIContainerEdit{
				Env:         env,
				DeviceNodes: commonNodes,
			},
		}
		spec.Devices = append(spec.Devices, dev)
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}

	targetPath := filepath.Join(d.cdiDir, fmt.Sprintf("%s.json", DriverName))
	tmpPath := targetPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, targetPath)
}
