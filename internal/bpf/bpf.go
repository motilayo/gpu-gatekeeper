package bpf

import (
	"github.com/cilium/ebpf"
)

// CgroupQuotaState matches struct cgroup_quota_state in bpf/headers/ggate.h.
type CgroupQuotaState struct {
	RefillRateTokensPerSec uint64
	MaxBurstTokens         uint64
	CurrentTokens          uint64
	DebtTokens             uint64
	LastUpdateNs           uint64
	TotalLaunches          uint64
	TotalThrottled         uint64
	TotalThrottledNs       uint64
	AllocatedVramBytes     uint64
	MaxVramBytes           uint64
	VramQuotaExceeded      uint64
}

// ThrottleEvent matches struct throttle_event in bpf/headers/ggate.h.
type ThrottleEvent struct {
	TimestampNs      uint64
	CgroupID         uint64
	Pid              uint32
	Tid              uint32
	DeficitTokens    uint64
	RequestedSleepNs uint64
}

// GgateObjects is an alias to the generated ggateObjects struct.
type GgateObjects = ggateObjects

// LoadGgateObjects loads the eBPF collection and assigns maps and programs to objs.
func LoadGgateObjects(objs *GgateObjects, opts *ebpf.CollectionOptions) error {
	return loadGgateObjects(objs, opts)
}
