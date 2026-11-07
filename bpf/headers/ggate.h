#ifndef __GGATE_H__
#define __GGATE_H__

#ifndef __VMLINUX_H__
#include <linux/types.h>
#endif

/*
 * struct cgroup_quota_state
 * Map value for cgroup_quota_map (BPF_MAP_TYPE_HASH).
 * Managed by userspace control plane; updated atomically by eBPF probes.
 */
struct cgroup_quota_state {
    __u64 refill_rate_tokens_per_sec; /* Configured by Go agent / K8s annotations */
    __u64 max_burst_tokens;           /* Bucket capacity */
    __u64 current_tokens;             /* Updated in eBPF */
    __u64 last_update_ns;             /* Updated via bpf_ktime_get_ns() */
    __u64 total_launches;             /* Kernel launch counter */
    __u64 total_throttled;            /* Throttle event counter */
    __u64 total_throttled_ns;         /* Cumulative throttling pause nanoseconds */
    __u64 allocated_vram_bytes;       /* Updated by cuMemAlloc_v2 probe */
    __u64 max_vram_bytes;             /* Configured VRAM quota in bytes (0 = unlimited) */
    __u64 vram_quota_exceeded;        /* Counter of allocations exceeding max_vram_bytes */
};

/*
 * struct throttle_event
 * Event payload submitted to events_ringbuf (BPF_MAP_TYPE_RINGBUF).
 * Consumed by userspace Go agent to wake throttled tasks after delay.
 */
struct throttle_event {
    __u64 timestamp_ns;       /* Nanosecond timestamp */
    __u64 cgroup_id;          /* Cgroup v2 inode */
    __u32 pid;                /* TGID (process ID in userspace) */
    __u32 tid;                /* PID (thread ID in userspace) */
    __u64 deficit_tokens;     /* Token deficit for current launch */
    __u64 requested_sleep_ns; /* Calculated pause duration for Go pacer */
};

/*
 * Prefix of CUDA's CUlaunchConfig struct passed to cuLaunchKernelEx.
 * First 5 fields represent the launch grid and block dimensions.
 */
struct cu_launch_config_prefix {
    __u32 gridDimX;
    __u32 gridDimY;
    __u32 gridDimZ;
    __u32 blockDimX;
    __u32 blockDimY;
};

/*
 * Helper structs for VRAM lifecycle deallocation tracking
 */
struct inflight_vram_alloc {
    __u64 dptr_ptr;
    __u64 bytesize;
};

struct vram_alloc_entry {
    __u64 cgroup_id;
    __u64 bytesize;
};

#endif /* __GGATE_H__ */
