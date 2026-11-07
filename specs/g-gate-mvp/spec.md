# G-Gate v2: eBPF GPU Attribution & QoS Governor Specification

## 1. Objective & Scope
G-Gate is an out-of-band, zero-injection GPU observability and Quality-of-Service (QoS) governor for Linux/Kubernetes. It intercepts user-space GPU driver API calls via eBPF `uprobes`, attributes GPU compute launches and VRAM allocations to `cgroup v2` inodes (Pods/Containers), exports per-tenant Prometheus metrics, and enforces soft/hard compute quotas via nanosecond token-bucket rate limiting and `SIGSTOP`/`SIGCONT` micro-pausing.

### Non-Goals for MVP
- Hooking kernel-space `/dev/nvidiactl` `ioctl` calls (bypasses user-mode submission queues).
- Mutating user containers or injecting `LD_PRELOAD` libraries.
- Requiring physical NVIDIA hardware for local CI/development (must run against a local `libcuda.so` mock library).

---

## 2. System Architecture

```
+-----------------------------------------------------------------------+
| User Space: Workload Process (PyTorch / Mock Runner inside cgroup v2) |
|   Calls cuLaunchKernel() / cuMemAlloc_v2() in libcuda.so              |
+-----------------------------------+-----------------------------------+
                                    | (eBPF uprobe trap)
                                    v
+-----------------------------------------------------------------------+
| Kernel Space: eBPF Data Plane (ggate_bpf.c)                           |
|   1. Reads cgroup_id = bpf_get_current_cgroup_id()                    |
|   2. Extracts gridDim/blockDim from CPU registers (PT_REGS_PARM2..6)  |
|   3. Updates BPF_MAP_TYPE_HASH (cgroup_state_map) token bucket        |
|   4. If tokens < cost: calls bpf_send_signal(SIGSTOP) & emits event   |
+-----------------------------------+-----------------------------------+
                                    | (BPF_MAP_TYPE_RINGBUF)
                                    v
+-----------------------------------------------------------------------+
| User Space: G-Gate Go Control Plane (cmd/ggate-agent)                 |
|   1. RingBuffer Consumer: Receives throttle & telemetry events        |
|   2. Pace Engine: Schedules time.AfterFunc(delay, kill(tgid, SIGCONT))|
|   3. Cgroup Watcher: Populates quota configurations per cgroup inode  |
|   4. Prometheus Exporter: Serves /metrics on :9090                    |
+-----------------------------------------------------------------------+
```

---

## 3. Data Model & eBPF Maps (`bpf/headers/ggate.h`)

### 3.1 `cgroup_quota_map` (`BPF_MAP_TYPE_HASH`)
Configured by the Go Control Plane; read/updated by the eBPF Data Plane.
- **Key:** `__u64 cgroup_id`
- **Value (`struct cgroup_quota_state`):**
  - `__u64 refill_rate_tokens_per_sec` (Configured by Go agent)
  - `__u64 max_burst_tokens` (Bucket capacity)
  - `__u64 current_tokens` (Updated in eBPF)
  - `__u64 last_update_ns` (Updated via `bpf_ktime_get_ns()`)
  - `__u64 total_launches` (Counter)
  - `__u64 total_throttled` (Counter)
  - `__u64 allocated_vram_bytes` (Updated by `cuMemAlloc_v2` probe)

### 3.2 `events_ringbuf` (`BPF_MAP_TYPE_RINGBUF`)
256 KB ring buffer streaming from eBPF to the Go agent.
- **Event Struct (`struct throttle_event`):**
  - `__u64 timestamp_ns`
  - `__u64 cgroup_id`
  - `__u32 pid` (Upper 32 bits of `bpf_get_current_pid_tgid()`)
  - `__u32 tid` (Lower 32 bits of `bpf_get_current_pid_tgid()`)
  - `__u64 deficit_tokens` (How many tokens short the caller was)
  - `__u64 requested_sleep_ns` (Calculated pause duration for Go pacer)

---

## 4. Target Hooks & Cross-Platform ABI Register Mapping (`arm64` AAPCS64 & `x86_64` SysV)

Both `arm64` (registers `x0`–`x5`) and `x86_64` (registers `rdi`, `rsi`, `rdx`, `rcx`, `r8`, `r9`) pass the first 6 integer/pointer arguments in CPU registers. Using `<bpf/bpf_tracing.h>` with `-target arm64,amd64` maps these portably across your M1 Pro Linux VM and x86_64 cloud GPU VMs:

### 4.1 Hook 1: `uprobe/cuLaunchKernel`
Target signature:
`CUresult cuLaunchKernel(CUfunction f, unsigned int gridDimX, unsigned int gridDimY, unsigned int gridDimZ, unsigned int blockDimX, unsigned int blockDimY, ...)`

- `PT_REGS_PARM2(ctx)` -> `gridDimX`
- `PT_REGS_PARM3(ctx)` -> `gridDimY`
- `PT_REGS_PARM4(ctx)` -> `gridDimZ`
- `PT_REGS_PARM5(ctx)` -> `blockDimX`
- `PT_REGS_PARM6(ctx)` -> `blockDimY`
- **Token Cost Formula:** `cost = max(1, (gridDimX * gridDimY * gridDimZ * blockDimX * blockDimY) >> 10)`

### 4.2 Hook 2: `uprobe/cuMemAlloc_v2`
Target signature:
`CUresult cuMemAlloc_v2(CUdeviceptr *dptr, size_t bytesize)`

- `PT_REGS_PARM2(ctx)` -> `bytesize`
- **Action:** Atomically add `bytesize` to `allocated_vram_bytes` in `cgroup_quota_map`.

---

## 5. Token Bucket & Zero-Crash Throttling Logic

On every `cuLaunchKernel` invocation for a tracked `cgroup_id`:
1. `now = bpf_ktime_get_ns()`
2. `delta_ns = now - state->last_update_ns`
3. `new_tokens = (delta_ns * state->refill_rate_tokens_per_sec) / 1000000000ULL`
4. `state->current_tokens = min(state->max_burst_tokens, state->current_tokens + new_tokens)`
5. `state->last_update_ns = now`
6. If `state->current_tokens >= cost`:
   - Deduct `cost` from `state->current_tokens`, increment `total_launches`, return `0`.
7. If `state->current_tokens < cost`:
   - Calculate `deficit = cost - state->current_tokens`.
   - Set `state->current_tokens = 0` and increment `total_throttled`.
   - Calculate `sleep_ns = (deficit * 1000000000ULL) / state->refill_rate_tokens_per_sec`.
   - Emit `struct throttle_event` to `events_ringbuf`.
   - Call `bpf_send_signal(SIGSTOP)` to pause the calling process before it writes to the GPU ring buffer.
   - Go agent reads `throttle_event`, waits `sleep_ns`, and sends `syscall.Kill(int(event.Pid), syscall.SIGCONT)`.

---

## 6. Local $0 Mock Harness (`mock/`)
To enable full local development and verification without an NVIDIA GPU:
1. `mock/libcuda_mock.c`: Shared library compiled to `build/libcuda.so` with `__attribute__((visibility("default"), noinline))` exporting `cuLaunchKernel` and `cuMemAlloc_v2`.
2. `mock/workload_sim.c`: Test binary linked against `build/libcuda.so` that runs a configurable loop of `cuLaunchKernel` calls and prints achieved launches/sec (IOPS).

---

## 7. Milestones & Acceptance Criteria

- **Milestone 1 (Mock Harness):** `make build-mock` produces `build/libcuda.so` and `build/workload_sim`. Running `nm -D build/libcuda.so` shows `cuLaunchKernel` and `cuMemAlloc_v2`.
- **Milestone 2 (eBPF Attribution):** `ggate-agent` attaches uprobes to `build/libcuda.so` and exports accurate `ggate_gpu_launches_total{cgroup_id="..."}` and `ggate_vram_allocated_bytes{cgroup_id="..."}` on `:9090/metrics`.
- **Milestone 3 (Token-Bucket Throttling):** Running two `workload_sim` processes in separate `cgroup v2` slices with a 20% / 80% token split throttles the 20% slice to within ±5% of its target launch rate with zero process crashes.
