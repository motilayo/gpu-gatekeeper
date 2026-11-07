# G-Gate Architecture

This document explains how G-Gate works internally, focusing on the four mechanisms that are hard to follow from code alone. For usage, see the [README](../README.md). For measured results, see the [Validation Report](../benchmarks/VALIDATION_REPORT.md).

---

## 1. Component Overview

G-Gate has two halves: an **eBPF data plane** that runs in the host kernel and makes per-call decisions, and a **Go control plane** that programs quotas into BPF maps, resumes paused tasks, and exports metrics.

```mermaid
flowchart TD
    subgraph Pod["Workload Pod (cgroup v2)"]
        APP["PyTorch / vLLM / CUDA app"]
        LIB["host libcuda.so.1"]
        APP --> LIB
    end

    subgraph Kernel["Host Kernel: eBPF Data Plane (bpf/ggate.bpf.c)"]
        UP["uprobes / uretprobes<br/>cuLaunchKernel, cuLaunchKernelEx, cuGraphLaunch,<br/>cuGraphAddKernelNode, cuGraphInstantiate,<br/>cuMemAlloc_v2, cuMemAllocAsync, cuMemFree_v2, cuMemFreeAsync"]
        QM[("cgroup_quota_map<br/>token bucket + VRAM state per cgroup_id")]
        VM[("vram_allocs_map<br/>GPU addr to cgroup_id, bytes")]
        GM[("graph_cost_map / graph_exec_cost_map")]
        RB[["events_ringbuf"]]
        UP --> QM
        UP --> VM
        UP --> GM
        UP --> RB
    end

    subgraph Agent["Go Control Plane (cmd/ggate-agent)"]
        RES["k8s Resolver<br/>Kubelet /pods + ResourceClaims"]
        DRA["DRA Driver<br/>unix socket, CDI spec, ResourceSlice"]
        PACER["Ringbuf reader + pacer<br/>time.AfterFunc then SIGCONT"]
        PROM["/metrics, /api/v1/tenants, /"]
    end

    LIB -. "probe fires on call" .-> UP
    RES -- "write quotas" --> QM
    DRA -- "cached claims" --> RES
    RB --> PACER
    PACER -- "SIGCONT" --> APP
    QM --> PROM
    KUBELET["Kubelet"] -- "NodePrepare / NodeUnprepare" --> DRA
```

**Key property:** nothing is injected into the container. Probes attach to the host's `libcuda.so.1` inode, so every process mapping that library is covered, whatever image it runs.

---

## 2. Compute Throttling Loop

This is the core mechanism. A kernel launch is charged tokens before it reaches the GPU's hardware queue. When the cgroup is out of tokens, the calling thread is stopped in user space so it can't flood the queue and hurt co-tenants.

```mermaid
sequenceDiagram
    participant App as App thread (in cgroup)
    participant BPF as uprobe/cuLaunchKernel
    participant Map as cgroup_quota_map
    participant RB as events_ringbuf
    participant Go as Go pacer
    participant GPU as GPU HW queue

    App->>BPF: cuLaunchKernel(grid, block, ...)
    BPF->>BPF: cgroup_id = bpf_get_current_cgroup_id()
    BPF->>BPF: cost = (grid x block) >> 10
    BPF->>Map: refill tokens if now > last_update_ns
    alt tokens >= cost and no debt
        BPF->>Map: tokens -= cost
        BPF-->>App: return (no-op)
        App->>GPU: launch proceeds
    else deficit
        BPF->>BPF: sleep = deficit / rate, capped at MAX_PAUSE_NS (50 ms)
        BPF->>Map: carry remaining deficit as debt_tokens
        BPF->>Map: last_update_ns = max(now, last) + sleep
        BPF->>RB: throttle_event{pid, cgroup_id, sleep_ns}
        BPF->>App: bpf_send_signal(SIGSTOP)
        Note over App: whole process stopped
        RB->>Go: event
        Go->>Go: time.AfterFunc(sleep_ns)
        Go->>App: SIGCONT
        Go->>Go: record time.Since(pausedAt) as wall-clock pause
        App->>GPU: launch proceeds
    end
```

Design notes:

- **Why move `last_update_ns` into the future:** a paused process must not earn tokens during its own penalty. Refill only happens when `now > last_update_ns`, so the sleep pays the deficit off exactly once.
- **Why cap pauses and carry debt:** one huge deficit shouldn't freeze a process for seconds and trip Kubernetes liveness probes. The deficit is split into ≤50 ms slices and `debt_tokens` makes sure nothing is forgiven. (50 ms is the smallest cap that still paces a 128-token CUDA Graph at 6,400 tok/s, which needs a 20 ms gap between launches.)
- **Why SIGSTOP from the kernel but SIGCONT from Go:** `bpf_send_signal` acts synchronously on the current task. A timed resume needs a user-space timer.
- **Wall-clock accounting:** `ggate_gpu_throttled_seconds_total` uses the measured `time.Since(pausedAt)`, not the requested sleep, so scheduler latency is counted too.

---

## 3. VRAM Lifecycle Tracking

The CUDA Driver API makes this harder than it looks: `cuMemAlloc_v2(CUdeviceptr *dptr, size_t bytes)` returns the GPU address through an **output pointer**, and `cuMemFree_v2(CUdeviceptr dptr)` passes **only the address, not the size**. To credit memory back correctly, G-Gate remembers each allocation by address.

```mermaid
sequenceDiagram
    participant App
    participant Entry as uprobe/cuMemAlloc_v2
    participant Ret as uretprobe/cuMemAlloc_v2
    participant Free as uprobe/cuMemFree_v2
    participant Inf as inflight_alloc_map (key: pid_tgid)
    participant VA as vram_allocs_map (key: GPU addr)
    participant Q as cgroup_quota_map

    App->>Entry: cuMemAlloc_v2(&dptr, bytes)
    Entry->>Q: allocated += bytes, check max_vram_bytes
    alt over quota
        Entry->>Q: vram_quota_exceeded++
        Entry-->>App: policy: audit (count) / throttle (SIGSTOP) / kill (SIGKILL)
    end
    Entry->>Inf: store {dptr pointer, bytes}
    Note over App: driver allocates and writes *dptr
    App->>Ret: return
    Ret->>Inf: lookup + delete by pid_tgid
    Ret->>Ret: addr = bpf_probe_read_user(dptr pointer)
    Ret->>VA: store addr -> {cgroup_id, bytes}
    App->>Free: cuMemFree_v2(addr)
    Free->>VA: lookup + delete addr
    Free->>Q: allocated -= bytes (on the owning cgroup)
```

`cuMemAllocAsync` / `cuMemFreeAsync` go through the same path. On a real Tesla T4, freeing a 258 MiB PyTorch tensor reduced the tracked allocation by exactly 270,532,608 bytes, with zero drift.

Note that `kill` policy acts at the *entry* probe, before the driver commits memory, so the over-budget process dies before it can cause a device-wide OOM.

---

## 4. CUDA Graph Costing

A `cuGraphLaunch(hGraphExec, stream)` call carries no grid/block dimensions, because the graph was built earlier. G-Gate sums each node's cost while the graph is built and attaches the total to the executable handle.

```mermaid
sequenceDiagram
    participant App
    participant Add as uprobe/cuGraphAddKernelNode
    participant Inst as u(ret)probe/cuGraphInstantiate
    participant Launch as uprobe/cuGraphLaunch
    participant GC as graph_cost_map (key: hGraph)
    participant EC as graph_exec_cost_map (key: hGraphExec)

    loop each kernel node
        App->>Add: cuGraphAddKernelNode(..., hGraph, ..., params)
        Add->>GC: cost[hGraph] += (grid x block) >> 10
    end
    App->>Inst: cuGraphInstantiate(&hGraphExec, hGraph, ...)
    Inst->>EC: on return: cost[hGraphExec] = cost[hGraph]
    loop each replay
        App->>Launch: cuGraphLaunch(hGraphExec, stream)
        Launch->>EC: lookup, else DEFAULT_GRAPH_LAUNCH_COST (32)
        Launch->>Launch: handle_launch(ctx, cost)
    end
```

Graphs captured by stream capture (`cuStreamBeginCapture`, which PyTorch's `torch.cuda.CUDAGraph` uses) don't go through `cuGraphAddKernelNode`, so they get the fallback cost of 32. Capture-path introspection is a known gap.

---

## 5. Kubernetes DRA Lifecycle Ordering

Kubelet calls `NodePrepareResources` **before** containerd creates the container's cgroup. That means no `cgroup_id` exists yet to bind a quota to. G-Gate solves this by caching the claim and binding it later.

```mermaid
sequenceDiagram
    participant K as Kubelet
    participant D as G-Gate DRA driver (unix socket)
    participant C as containerd
    participant R as Resolver (periodic SyncOnce)
    participant Q as cgroup_quota_map

    K->>D: POST /v1/NodePrepareResources {claim uid, tokensPerSec, maxVramBytes, vramPolicy}
    D->>D: cache PreparedClaim (by UID and namespace/name)
    D->>D: write /var/run/cdi/gpu.g-gate.io.json (env + device nodes if /dev/nvidiactl exists)
    D-->>K: cdiDevices: ["gpu.g-gate.io/gpu=claim-uid"]
    K->>C: create container with CDI device
    C->>C: mkdir cgroup v2 dir (new inode)
    R->>R: scan /sys/fs/cgroup, match pod UID + container ID
    R->>D: lookup claim (cache first, then resource.k8s.io v1, v1beta1 fallback)
    R->>Q: write quota for cgroup_id
    Note over Q: enforcement active from first matched sync
    K->>D: POST /v1/NodeUnprepareResources {claimUIDs}
    D->>D: drop cache entry, rewrite CDI spec
```

Quota sources, in priority order: DRA `ResourceClaim` (cache or API), then Pod annotations (`g-gate.io/tokens-per-sec`, `g-gate.io/max-vram-mb`, `g-gate.io/vram-policy`), then static `--quota` flags.

---

## 6. Design Decisions and Known Limits

| Decision | Why | Trade-off |
| :--- | :--- | :--- |
| eBPF uprobes on host `libcuda.so.1` instead of `LD_PRELOAD` | No container mutation. Workloads can't bypass or tamper with it. Works with distroless images. | Requires root/`CAP_BPF` on the node and kernel BTF. |
| Throttle at **submission**, not execution | The GPU can't easily preempt queued work, so the only point where QoS can be enforced on non-MIG GPUs is before work enters the queue. | Can't preempt a kernel that is already running. |
| Cost proxy `(gridDim x blockDim) >> 10` | Cheap to compute in-kernel. Tracks real share well for framework workloads that launch many small kernels (99.57 to 99.98% on T4). | A deliberately pathological kernel (tiny grid, long runtime) is under-charged. |
| `SIGSTOP` the whole process | The only preemption primitive available synchronously from a uprobe. | Pauses all threads in the process, not just the launching thread. |
| Default graph cost 32 | Safe fallback when construction isn't observed. | Stream-captured graphs aren't costed by node yet. |
| Periodic resolver sync | Simple and robust to missed events. | Up to one sync interval between cgroup creation and enforcement. Unenrolled cgroups run unthrottled (attribution still works). |

### Out of scope (by design)

- **Hard SM or memory-bandwidth isolation.** Use MIG where hardware supports it.
- **Non-CUDA runtimes.** ROCm (`libamdhip64.so`) uses the same approach but isn't implemented.
- **Multi-GPU-per-process attribution.** Quotas are per cgroup, not per device.
