package dra

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

	var commonNodes []CDIDeviceNode
	for _, dev := range []string{"/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools", "/dev/nvidia0"} {
		if _, err := os.Stat(dev); err == nil {
			commonNodes = append(commonNodes, CDIDeviceNode{
				Path:     dev,
				HostPath: dev,
			})
		}
	}

	for uid, claim := range d.claims {
		env := []string{
			fmt.Sprintf("G_GATE_CLAIM_UID=%s", uid),
			fmt.Sprintf("G_GATE_TOKENS_PER_SEC=%d", claim.TokensPerSec),
			fmt.Sprintf("G_GATE_BURST_TOKENS=%d", claim.BurstTokens),
			fmt.Sprintf("G_GATE_MAX_VRAM_BYTES=%d", claim.MaxVramBytes),
		}
		if claim.ClaimName != "" {
			env = append(env, fmt.Sprintf("G_GATE_CLAIM_NAME=%s", claim.ClaimName))
		}
		if claim.Namespace != "" {
			env = append(env, fmt.Sprintf("G_GATE_NAMESPACE=%s", claim.Namespace))
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
