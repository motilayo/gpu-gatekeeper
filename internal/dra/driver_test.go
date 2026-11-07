package dra

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDRADriverPrepareAndUnprepareCDISpec(t *testing.T) {
	tmpCDIDir := t.TempDir()
	driver := NewDriver(tmpCDIDir)

	claim := PreparedClaim{
		ClaimUID:     "1111-2222",
		ClaimName:    "llama3-gpu-claim",
		Namespace:    "ai-prod",
		DeviceID:     "gpu-0",
		TokensPerSec: 6400,
		BurstTokens:  64,
		MaxVramBytes: 32 * 1024 * 1024,
	}

	cdiDeviceID, err := driver.NodePrepareResource(claim)
	if err != nil {
		t.Fatalf("NodePrepareResource failed: %v", err)
	}

	expectedCDIDeviceID := "gpu.g-gate.io/gpu=claim-1111-2222"
	if cdiDeviceID != expectedCDIDeviceID {
		t.Fatalf("expected CDIDeviceID %q, got %q", expectedCDIDeviceID, cdiDeviceID)
	}

	// Verify CDI spec was written to disk
	cdiFilePath := filepath.Join(tmpCDIDir, "gpu.g-gate.io.json")
	data, err := os.ReadFile(cdiFilePath)
	if err != nil {
		t.Fatalf("failed to read CDI spec file: %v", err)
	}

	var spec CDISpec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("failed to parse CDI spec JSON: %v", err)
	}

	if spec.CDIVersion != CDIVersion {
		t.Errorf("CDIVersion mismatch: got %q, want %q", spec.CDIVersion, CDIVersion)
	}
	if spec.Kind != CDIKind {
		t.Errorf("Kind mismatch: got %q, want %q", spec.Kind, CDIKind)
	}
	if len(spec.Devices) != 1 {
		t.Fatalf("expected 1 device in CDI spec, got %d", len(spec.Devices))
	}

	device := spec.Devices[0]
	if device.Name != "claim-1111-2222" {
		t.Errorf("device name mismatch: got %q, want %q", device.Name, "claim-1111-2222")
	}

	foundTokensEnv := false
	foundResourceClaimEnv := false
	foundClaimUIDEnv := false
	for _, env := range device.ContainerEdits.Env {
		if strings.HasPrefix(env, "G_GATE_TOKENS_PER_SEC=") {
			foundTokensEnv = true
			if env != "G_GATE_TOKENS_PER_SEC=6400" {
				t.Errorf("unexpected env: got %q, want %q", env, "G_GATE_TOKENS_PER_SEC=6400")
			}
		}
		if env == "GGATE_RESOURCE_CLAIM=llama3-gpu-claim" {
			foundResourceClaimEnv = true
		}
		if env == "GGATE_CLAIM_UID=1111-2222" {
			foundClaimUIDEnv = true
		}
	}
	if !foundTokensEnv {
		t.Errorf("G_GATE_TOKENS_PER_SEC environment variable missing in CDI edits")
	}
	if !foundResourceClaimEnv {
		t.Errorf("GGATE_RESOURCE_CLAIM environment variable missing in CDI edits")
	}
	if !foundClaimUIDEnv {
		t.Errorf("GGATE_CLAIM_UID environment variable missing in CDI edits")
	}

	// If /dev/nvidiactl is not present on this host, deviceNodes must be empty/omitted
	if _, err := os.Stat("/dev/nvidiactl"); err != nil {
		if len(device.ContainerEdits.DeviceNodes) != 0 {
			t.Errorf("expected 0 device nodes when /dev/nvidiactl is absent, got %d", len(device.ContainerEdits.DeviceNodes))
		}
	}

	// Verify GetPreparedClaim & GetPreparedClaimByName
	savedClaim, found := driver.GetPreparedClaim("1111-2222")
	if !found || savedClaim.TokensPerSec != 6400 {
		t.Fatalf("GetPreparedClaim failed or returned invalid data: %+v", savedClaim)
	}

	byNameClaim, foundByName := driver.GetPreparedClaimByName("ai-prod", "llama3-gpu-claim")
	if !foundByName || byNameClaim.ClaimUID != "1111-2222" {
		t.Fatalf("GetPreparedClaimByName failed or returned invalid data: %+v", byNameClaim)
	}

	// Test NodeUnprepareResource
	if err := driver.NodeUnprepareResource("1111-2222"); err != nil {
		t.Fatalf("NodeUnprepareResource failed: %v", err)
	}

	// Check that spec now has 0 devices
	dataAfter, err := os.ReadFile(cdiFilePath)
	if err != nil {
		t.Fatalf("failed to read CDI spec file after unprepare: %v", err)
	}
	var specAfter CDISpec
	if err := json.Unmarshal(dataAfter, &specAfter); err != nil {
		t.Fatalf("failed to parse CDI spec after unprepare: %v", err)
	}
	if len(specAfter.Devices) != 0 {
		t.Errorf("expected 0 devices after unprepare, got %d", len(specAfter.Devices))
	}
}

func TestDRASocketServerPrepareAndUnprepare(t *testing.T) {
	// Use /tmp to avoid macOS 104-character sockaddr_un limit
	tmpDir, err := os.MkdirTemp("/tmp", "dra-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir in /tmp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cdiDir := filepath.Join(tmpDir, "cdi")
	sockPath := filepath.Join(tmpDir, "dra.sock")

	driver := NewDriver(cdiDir)
	stopServer, err := driver.StartSocketServer(sockPath)
	if err != nil {
		t.Fatalf("StartSocketServer failed: %v", err)
	}
	defer func() {
		_ = stopServer()
	}()

	// Unix socket HTTP client
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}

	// 1. Test POST /v1/NodePrepareResources
	prepareBody := `{
		"claims": [
			{
				"uid": "dra-claim-uid-01",
				"name": "vllm-dra-claim-01",
				"namespace": "ai-prod",
				"tokensPerSec": 6400,
				"burstTokens": 64,
				"maxVramBytes": 33554432,
				"vramPolicy": "audit"
			}
		]
	}`

	resp, err := client.Post("http://unix/v1/NodePrepareResources", "application/json", strings.NewReader(prepareBody))
	if err != nil {
		t.Fatalf("POST /v1/NodePrepareResources failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var prepResp NodePrepareResponse
	if err := json.NewDecoder(resp.Body).Decode(&prepResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	claimResult, ok := prepResp.Claims["dra-claim-uid-01"]
	if !ok {
		t.Fatalf("missing claim dra-claim-uid-01 in response: %+v", prepResp)
	}
	expectedCDIDevice := "gpu.g-gate.io/gpu=claim-dra-claim-uid-01"
	if len(claimResult.CDIDevices) != 1 || claimResult.CDIDevices[0] != expectedCDIDevice {
		t.Fatalf("expected CDIDevices [%s], got %+v", expectedCDIDevice, claimResult.CDIDevices)
	}

	// Verify in-memory cache
	cachedClaim, found := driver.GetPreparedClaim("dra-claim-uid-01")
	if !found {
		t.Fatalf("expected claim in driver memory cache, but not found")
	}
	if cachedClaim.TokensPerSec != 6400 || cachedClaim.BurstTokens != 64 || cachedClaim.MaxVramBytes != 33554432 {
		t.Fatalf("cached claim mismatch: %+v", cachedClaim)
	}

	// Verify CDI spec was generated on disk
	cdiFile := filepath.Join(cdiDir, "gpu.g-gate.io.json")
	cdiData, err := os.ReadFile(cdiFile)
	if err != nil {
		t.Fatalf("failed to read generated CDI spec file: %v", err)
	}
	if !strings.Contains(string(cdiData), "claim-dra-claim-uid-01") {
		t.Fatalf("CDI spec does not contain claim-dra-claim-uid-01: %s", string(cdiData))
	}

	// 2. Test POST /v1/NodeUnprepareResources
	unprepareBody := `{"claimUIDs": ["dra-claim-uid-01"]}`
	unprepResp, err := client.Post("http://unix/v1/NodeUnprepareResources", "application/json", strings.NewReader(unprepareBody))
	if err != nil {
		t.Fatalf("POST /v1/NodeUnprepareResources failed: %v", err)
	}
	defer unprepResp.Body.Close()

	if unprepResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(unprepResp.Body)
		t.Fatalf("unexpected unprepare status %d: %s", unprepResp.StatusCode, string(body))
	}

	var unprepResult NodeUnprepareResponse
	if err := json.NewDecoder(unprepResp.Body).Decode(&unprepResult); err != nil {
		t.Fatalf("failed to decode unprepare response: %v", err)
	}
	if len(unprepResult.Unprepared) != 1 || unprepResult.Unprepared[0] != "dra-claim-uid-01" {
		t.Fatalf("expected Unprepared [dra-claim-uid-01], got %+v", unprepResult.Unprepared)
	}

	// Verify in-memory cache cleared
	if _, found := driver.GetPreparedClaim("dra-claim-uid-01"); found {
		t.Fatalf("claim dra-claim-uid-01 should have been deleted from memory cache")
	}

	// Verify CDI spec has 0 devices
	cdiDataAfter, err := os.ReadFile(cdiFile)
	if err != nil {
		t.Fatalf("failed to read CDI spec after unprepare: %v", err)
	}
	var specAfter CDISpec
	if err := json.Unmarshal(cdiDataAfter, &specAfter); err != nil {
		t.Fatalf("failed to unmarshal CDI spec after unprepare: %v", err)
	}
	if len(specAfter.Devices) != 0 {
		t.Fatalf("expected 0 devices after unprepare, got %d", len(specAfter.Devices))
	}
}

func TestPublishResourceSliceHTTP(t *testing.T) {
	var receivedAuth string
	var receivedBody []byte

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r.Body)
		receivedBody = buf.Bytes()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status":"Success"}`))
	}))
	defer ts.Close()

	err := PublishResourceSliceHTTP(ts.URL, "test-bearer-token", "test-node", 400000, 16*1024*1024*1024)
	if err != nil {
		t.Fatalf("PublishResourceSliceHTTP failed: %v", err)
	}

	if receivedAuth != "Bearer test-bearer-token" {
		t.Errorf("expected Authorization 'Bearer test-bearer-token', got %q", receivedAuth)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(receivedBody, &parsed); err != nil {
		t.Fatalf("failed to parse received ResourceSlice JSON: %v", err)
	}
	if parsed["apiVersion"] != "resource.k8s.io/v1" {
		t.Errorf("unexpected apiVersion: %v", parsed["apiVersion"])
	}
	if parsed["kind"] != "ResourceSlice" {
		t.Errorf("unexpected kind: %v", parsed["kind"])
	}
	spec, ok := parsed["spec"].(map[string]interface{})
	if !ok || spec["nodeName"] != "test-node" {
		t.Errorf("unexpected spec.nodeName: %v", spec["nodeName"])
	}
}

func TestWriteResourceSliceFile(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "sub", "resourceslice.json")
	if err := WriteResourceSliceFile(tmpFile, "node-abc", 500000, 8*1024*1024*1024); err != nil {
		t.Fatalf("WriteResourceSliceFile failed: %v", err)
	}

	data, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("failed to read written ResourceSlice file: %v", err)
	}

	var slice map[string]interface{}
	if err := json.Unmarshal(data, &slice); err != nil {
		t.Fatalf("failed to parse ResourceSlice JSON: %v", err)
	}
	if slice["kind"] != "ResourceSlice" {
		t.Errorf("expected kind ResourceSlice, got %v", slice["kind"])
	}
}
