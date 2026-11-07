#include "vmlinux.h"

#if defined(__TARGET_ARCH_x86)
struct pt_regs_x86 {
    unsigned long r15;
    unsigned long r14;
    unsigned long r13;
    unsigned long r12;
    unsigned long bp;
    unsigned long bx;
    unsigned long r11;
    unsigned long r10;
    unsigned long r9;
    unsigned long r8;
    unsigned long ax;
    unsigned long cx;
    unsigned long dx;
    unsigned long si;
    unsigned long di;
    unsigned long orig_ax;
    unsigned long ip;
    unsigned long cs;
    unsigned long flags;
    unsigned long sp;
    unsigned long ss;
};
#define __PT_REGS_CAST(x) ((const struct pt_regs_x86 *)(x))
#elif defined(__TARGET_ARCH_arm64)
#define user_pt_regs user_pt_regs_arm64
struct user_pt_regs_arm64 {
    __u64 regs[31];
    __u64 sp;
    __u64 pc;
    __u64 pstate;
};
#endif

#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include "headers/ggate.h"

#define SIGSTOP 19
#define MAX_PAUSE_NS 10000000ULL          /* 10 ms ceiling per single pause slice */
#define DEFAULT_GRAPH_LAUNCH_COST 32ULL   /* Default token weight for a cuGraphLaunch replay */

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 4096);
    __type(key, __u64);
    __type(value, struct cgroup_quota_state);
} cgroup_quota_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 256 * 1024);
} events_ringbuf SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 4096);
    __type(key, __u64);
    __type(value, struct inflight_vram_alloc);
} inflight_alloc_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 65536);
    __type(key, __u64);
    __type(value, struct vram_alloc_entry);
} vram_allocs_map SEC(".maps");

static __always_inline int handle_launch(struct pt_regs *ctx, __u64 cost)
{
    __u64 cgroup_id = bpf_get_current_cgroup_id();
    if (cost < 1)
        cost = 1;

    struct cgroup_quota_state *state = bpf_map_lookup_elem(&cgroup_quota_map, &cgroup_id);
    if (!state) {
        struct cgroup_quota_state init_state = {};
        init_state.total_launches = 1;
        init_state.last_update_ns = bpf_ktime_get_ns();
        bpf_map_update_elem(&cgroup_quota_map, &cgroup_id, &init_state, BPF_NOEXIST);
        return 0;
    }

    /* Attribution-only mode when refill rate is zero; guards against division by zero */
    __u64 rate = state->refill_rate_tokens_per_sec;
    if (rate == 0) {
        __sync_fetch_and_add(&state->total_launches, 1);
        return 0;
    }

    __u64 now = bpf_ktime_get_ns();
    if (state->last_update_ns == 0) {
        state->last_update_ns = now;
    }

    if (now > state->last_update_ns) {
        __u64 delta_ns = now - state->last_update_ns;
        if (delta_ns > 10000000000ULL)
            delta_ns = 10000000000ULL;

        __u64 new_tokens = (delta_ns * rate) / 1000000000ULL;
        if (new_tokens > 0) {
            if (state->debt_tokens > 0) {
                if (new_tokens >= state->debt_tokens) {
                    new_tokens -= state->debt_tokens;
                    state->debt_tokens = 0;
                } else {
                    state->debt_tokens -= new_tokens;
                    new_tokens = 0;
                }
            }
            __u64 current_tokens = state->current_tokens + new_tokens;
            if (current_tokens > state->max_burst_tokens)
                current_tokens = state->max_burst_tokens;
            state->current_tokens = current_tokens;
            state->last_update_ns = now;
        }
    }

    if (state->debt_tokens == 0 && state->current_tokens >= cost) {
        state->current_tokens -= cost;
        if (now > state->last_update_ns) {
            state->last_update_ns = now;
        }
        __sync_fetch_and_add(&state->total_launches, 1);
        return 0;
    }

    /* Throttling path: deficit calculation and micro-sliced pause signal */
    __u64 total_needed = cost + state->debt_tokens;
    __u64 deficit = (total_needed > state->current_tokens) ? (total_needed - state->current_tokens) : 1;
    state->current_tokens = 0;
    __sync_fetch_and_add(&state->total_launches, 1);
    __sync_fetch_and_add(&state->total_throttled, 1);

    __u64 uncapped_sleep_ns = (deficit * 1000000000ULL) / rate;
    if (uncapped_sleep_ns == 0)
        uncapped_sleep_ns = 100000ULL;

    __u64 capped_sleep_ns = uncapped_sleep_ns;
    if (capped_sleep_ns > MAX_PAUSE_NS) {
        capped_sleep_ns = MAX_PAUSE_NS;
        __u64 paid_tokens = (capped_sleep_ns * rate) / 1000000000ULL;
        if (deficit > paid_tokens) {
            state->debt_tokens = deficit - paid_tokens;
        } else {
            state->debt_tokens = 0;
        }
    } else {
        state->debt_tokens = 0;
    }

    __sync_fetch_and_add(&state->total_throttled_ns, capped_sleep_ns);
    /* Advance last_update_ns from max(now, last_update_ns) by capped_sleep_ns */
    __u64 base_ns = (now > state->last_update_ns) ? now : state->last_update_ns;
    state->last_update_ns = base_ns + capped_sleep_ns;

    struct throttle_event *ev = bpf_ringbuf_reserve(&events_ringbuf, sizeof(struct throttle_event), 0);
    if (ev) {
        ev->timestamp_ns = now;
        ev->cgroup_id = cgroup_id;
        __u64 pid_tgid = bpf_get_current_pid_tgid();
        ev->pid = (__u32)(pid_tgid >> 32);
        ev->tid = (__u32)(pid_tgid & 0xFFFFFFFF);
        ev->deficit_tokens = deficit;
        ev->requested_sleep_ns = capped_sleep_ns;
        bpf_ringbuf_submit(ev, 0);
    }

    bpf_send_signal(SIGSTOP);
    return 0;
}

static __always_inline int process_kernel_launch(struct pt_regs *ctx, __u32 gridDimX, __u32 gridDimY, __u32 gridDimZ, __u32 blockDimX, __u32 blockDimY)
{
    __u64 cost = ((__u64)gridDimX * gridDimY * gridDimZ * blockDimX * blockDimY) >> 10;
    if (cost < 1)
        cost = 1;
    return handle_launch(ctx, cost);
}

SEC("uprobe/cuLaunchKernel")
int handle_cu_launch_kernel(struct pt_regs *ctx)
{
    __u32 gridDimX = (__u32)PT_REGS_PARM2(ctx);
    __u32 gridDimY = (__u32)PT_REGS_PARM3(ctx);
    __u32 gridDimZ = (__u32)PT_REGS_PARM4(ctx);
    __u32 blockDimX = (__u32)PT_REGS_PARM5(ctx);
    __u32 blockDimY = (__u32)PT_REGS_PARM6(ctx);

    return process_kernel_launch(ctx, gridDimX, gridDimY, gridDimZ, blockDimX, blockDimY);
}

SEC("uprobe/cuLaunchKernelEx")
int handle_cu_launch_kernel_ex(struct pt_regs *ctx)
{
    struct cu_launch_config_prefix cfg = {};
    const void *cfg_ptr = (const void *)PT_REGS_PARM1(ctx);
    if (cfg_ptr != 0) {
        bpf_probe_read_user(&cfg, sizeof(cfg), cfg_ptr);
    }
    return process_kernel_launch(ctx, cfg.gridDimX, cfg.gridDimY, cfg.gridDimZ, cfg.blockDimX, cfg.blockDimY);
}

SEC("uprobe/cuGraphLaunch")
int handle_cu_graph_launch(struct pt_regs *ctx)
{
    return handle_launch(ctx, DEFAULT_GRAPH_LAUNCH_COST);
}

static __always_inline int process_mem_alloc(__u64 dptr_ptr, __u64 bytesize)
{
    __u64 cgroup_id = bpf_get_current_cgroup_id();

    struct cgroup_quota_state *state = bpf_map_lookup_elem(&cgroup_quota_map, &cgroup_id);
    if (!state) {
        struct cgroup_quota_state init_state = {};
        init_state.allocated_vram_bytes = bytesize;
        init_state.last_update_ns = bpf_ktime_get_ns();
        bpf_map_update_elem(&cgroup_quota_map, &cgroup_id, &init_state, BPF_NOEXIST);
    } else {
        __u64 prev = __sync_fetch_and_add(&state->allocated_vram_bytes, bytesize);
        if (state->max_vram_bytes > 0 && (prev + bytesize) > state->max_vram_bytes) {
            __sync_fetch_and_add(&state->vram_quota_exceeded, 1);
        }
    }

    __u64 pid_tgid = bpf_get_current_pid_tgid();
    struct inflight_vram_alloc inflight = {
        .dptr_ptr = dptr_ptr,
        .bytesize = bytesize,
    };
    bpf_map_update_elem(&inflight_alloc_map, &pid_tgid, &inflight, BPF_ANY);

    return 0;
}

static __always_inline int process_mem_alloc_ret(void)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    struct inflight_vram_alloc *inflight = bpf_map_lookup_elem(&inflight_alloc_map, &pid_tgid);
    if (!inflight)
        return 0;

    __u64 dptr_ptr = inflight->dptr_ptr;
    __u64 bytesize = inflight->bytesize;
    bpf_map_delete_elem(&inflight_alloc_map, &pid_tgid);

    __u64 addr = 0;
    if (dptr_ptr != 0) {
        bpf_probe_read_user(&addr, sizeof(addr), (const void *)dptr_ptr);
    }
    if (addr != 0) {
        struct vram_alloc_entry entry = {
            .cgroup_id = bpf_get_current_cgroup_id(),
            .bytesize = bytesize,
        };
        bpf_map_update_elem(&vram_allocs_map, &addr, &entry, BPF_ANY);
    }

    return 0;
}

static __always_inline int process_mem_free(__u64 addr)
{
    struct vram_alloc_entry *entry = bpf_map_lookup_elem(&vram_allocs_map, &addr);
    if (!entry)
        return 0;

    __u64 cgroup_id = entry->cgroup_id;
    __u64 bytesize = entry->bytesize;
    bpf_map_delete_elem(&vram_allocs_map, &addr);

    struct cgroup_quota_state *state = bpf_map_lookup_elem(&cgroup_quota_map, &cgroup_id);
    if (!state)
        return 0;

    __sync_fetch_and_sub(&state->allocated_vram_bytes, bytesize);
    return 0;
}

SEC("uprobe/cuMemAlloc_v2")
int handle_cu_mem_alloc(struct pt_regs *ctx)
{
    return process_mem_alloc((__u64)PT_REGS_PARM1(ctx), (__u64)PT_REGS_PARM2(ctx));
}

SEC("uretprobe/cuMemAlloc_v2")
int handle_cu_mem_alloc_ret(struct pt_regs *ctx)
{
    return process_mem_alloc_ret();
}

SEC("uprobe/cuMemFree_v2")
int handle_cu_mem_free(struct pt_regs *ctx)
{
    return process_mem_free((__u64)PT_REGS_PARM1(ctx));
}

SEC("uprobe/cuMemAllocAsync")
int handle_cu_mem_alloc_async(struct pt_regs *ctx)
{
    return process_mem_alloc((__u64)PT_REGS_PARM1(ctx), (__u64)PT_REGS_PARM2(ctx));
}

SEC("uretprobe/cuMemAllocAsync")
int handle_cu_mem_alloc_async_ret(struct pt_regs *ctx)
{
    return process_mem_alloc_ret();
}

SEC("uprobe/cuMemFreeAsync")
int handle_cu_mem_free_async(struct pt_regs *ctx)
{
    return process_mem_free((__u64)PT_REGS_PARM1(ctx));
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
