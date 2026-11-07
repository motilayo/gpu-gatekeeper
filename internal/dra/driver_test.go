package dra

import (
	"encoding/json"
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
