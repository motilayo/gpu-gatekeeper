# G-Gate Public Launch Playbook & Technical Post Drafts

This document contains ready-to-publish launch copy for **Hacker News (`Show HN`)**, **`r/kubernetes` / `r/LocalLLaMA`**, **CNCF Slack (`#wg-device-management`)**, and the companion technical post: ***"Why Kubernetes DRA Needs an eBPF Data Plane"***.

---

## 1. Pre-Flight Checklist (2 Minutes Before Posting)

1. **Make the GitHub Repository Public:**
   ```bash
   gh repo edit motilayo/gpu-gatekeeper --visibility public --accept-visibility-change-consequences
   ```
2. **Make the GHCR Container Package Public:**
   - Open `https://github.com/users/motilayo/packages/container/gpu-gatekeeper/settings`
   - Scroll to **Danger Zone -> Change visibility -> Public** so `helm install` pulls `ghcr.io/motilayo/gpu-gatekeeper:v0.5.1` without authentication.

---

## 2. Hacker News (`Show HN`) Submission

**Title:**
`Show HN: G-Gate – Out-of-band eBPF GPU QoS governor and K8s 1.34 DRA driver`

**URL:**
`https://github.com/motilayo/gpu-gatekeeper`

**First Comment (Post immediately after submitting):**

> Hi HN,
>
> I built **G-Gate** (`Apache-2.0`), an out-of-band eBPF GPU observability and Quality-of-Service (QoS) governor for Linux and Kubernetes (`arm64` + `x86_64`), paired with a native Kubernetes 1.34 Dynamic Resource Allocation (`resource.k8s.io/v1`) driver and CDI `0.6.0` spec generator.
>
> ### The Problem
> If you have $30,000 H100s or A100s and need up to 7 static slices, you use hardware MIG.
> If you run inference, notebooks, or batch jobs on non-MIG GPUs (`T4`, `L4`, `L40S`, `A10G`, RTX) in Kubernetes, sharing one GPU across pods currently means choosing between:
> 1. **Blind time-slicing / MPS:** No per-pod compute QoS and no per-pod VRAM isolation. A noisy neighbor running batch prefill or a runaway `torch.empty()` either floods the GPU's hardware command queue (spiking p99 tail latency for co-located pods) or triggers a device-wide CUDA OOM that crashes neighboring containers.
> 2. **`LD_PRELOAD` interceptors (`HAMi-core`, `vcuda`):** Injects a custom C shared library into every container via `LD_PRELOAD` to wrap `cuLaunchKernel` and `cuMemAlloc`. Because it runs inside the container's own unprivileged address space, it mutates pod specs, breaks distroless/custom images, and has no security boundary (`unset LD_PRELOAD`).
>
> ### How G-Gate Works
> G-Gate runs **100% outside the container in the host Linux kernel** via eBPF `uprobes`/`uretprobes` attached to the host's `libcuda.so.1`:
> - **Per-Pod & Per-`ResourceClaim` Attribution:** Intercepts `cuLaunchKernel`, `cuLaunchKernelEx`, `cuGraphAddKernelNode`, `cuGraphInstantiate`, `cuGraphLaunch`, `cuMemAlloc_v2`, `cuMemAllocAsync`, `cuMemFree_v2`, and `cuMemFreeAsync`, attributing every launch and byte of VRAM to its `cgroup v2` inode (`bpf_get_current_cgroup_id()`) and Kubernetes Pod / `ResourceClaim`.
> - **Compute Pacing Before the Hardware Queue:** GPU hardware command queues are deep; once a noisy neighbor floods the queue, the GPU cannot easily preempt those kernels. G-Gate's in-kernel nanosecond token bucket (`cgroup_quota_map`) charges `(gridDim * blockDim) >> 10` tokens per launch (or accumulated multi-node cost for `cuGraphLaunch`) and micro-pauses over-budget processes in user space (`SIGSTOP`/`SIGCONT` capped at `50 ms` slices with debt carry) *before* they can flood the hardware queue.
> - **Byte-Exact VRAM Lifecycle & Active OOM Protection (`--vram-policy=audit|throttle|kill`):** In the CUDA Driver API, `cuMemFree_v2(CUdeviceptr dptr)` only passes the 64-bit GPU address, not the size. G-Gate caches the output pointer in `uprobe/cuMemAlloc_v2`, dereferences the returned GPU address in `uretprobe/cuMemAlloc_v2`, stores `addr -> {cgroup_id, bytesize}` in a BPF map, and decrements the owning cgroup's gauge on `uprobe/cuMemFree_v2`. Under `--vram-policy=kill`, an over-budget allocation immediately receives an in-kernel `bpf_send_signal(SIGKILL)` before it can OOM co-tenants.
> - **Native Kubernetes 1.34 DRA (`resource.k8s.io/v1`):** Includes a Kubelet DRA Unix socket server (`/v1/NodePrepareResources`), `ResourceSlice` publisher, and CDI `0.6.0` generator so pods claim GPU token rates and VRAM budgets natively without custom annotations.
>
> ### Real-Hardware Validation on NVIDIA Tesla T4 (Plus Where It Hurts)
> All raw CSVs, JSON latency histograms, pre-registered predictions, and a `<$0.05` 15-minute AWS Spot reproduction script (`deploy/cloud/aws-spot-smoke.sh`) are in [`benchmarks/`](https://github.com/motilayo/gpu-gatekeeper/blob/main/benchmarks/VALIDATION_REPORT.md):
> - **Probe Overhead:** `+2.40 µs` median latency per launch at `15.2k` launches/sec (`65.03 µs` native vs `67.43 µs` G-Gate).
> - **20/80 Priority Split (PyTorch `2.7.0+cu128` GEMMs):** `1,250.81` vs `5,024.81` IOPS (`0.24893` ratio vs `0.25000` target = **`99.57%` precision**). More importantly, `tenant-80`'s median launch latency is **`56.7 µs`** (below the `65.03 µs` single-tenant baseline) while `tenant-20` absorbs the queueing delay (`952.6 µs`).
> - **10 / 30 / 60 3-Tenant Split:** `10.005% : 30.016% : 59.980%` (**`99.92%` precision**, CV `<0.022%`).
> - **CUDA Graph Replay (`torch.cuda.CUDAGraph` via `uprobe/cuGraphLaunch`):** `196.71` vs `789.63` replays/sec (**`99.65%` precision**).
> - **VRAM Reclamation:** `258.00 MiB` PyTorch tensor freed via `torch.cuda.empty_cache()` -> exact `270,532,608 B` (`258.000000 MiB`) reclaimed with **`0 B` drift**.
>
> ### Honest Limitations & The Eager-Mode `uprobe` Amplification Lesson
> We also pre-registered hypotheses ([`PREDICTIONS.md`](https://github.com/motilayo/gpu-gatekeeper/blob/main/benchmarks/results/llm/PREDICTIONS.md)) and tested real autoregressive LLM decoding (`Qwen2.5-0.5B` FP16):
> 1. **Eager-Mode Launch Frequency vs. CUDA Graphs:** Uncompiled eager-mode Hugging Face `transformers` dispatches `~1,640` tiny CUDA kernels *per generated token*. At `~3.5 µs` per Linux `uprobe` trap, $1,640 \times 3.5\ \mu\text{s} \approx 5.7\text{ ms/token}$ (~16% overhead on a 0.5B model even with 0 throttles). When decode steps are captured into a **CUDA Graph** (`cuGraphLaunch`, which vLLM and TensorRT-LLM use by default), those 1,640 traps collapse into **1 `uprobe` trap per token (`3.5 µs`)**.
> 2. **Submission-Time Pacing Only:** G-Gate paces work *before* it enters the GPU hardware queue; it cannot preempt a kernel that is already executing on SMs, and a pathological `1x1` grid kernel with an infinite loop is under-charged by `(gridDim * blockDim) >> 10`.
> 3. **Stream-Captured Graph Costing:** Graphs built via `cuGraphAddKernelNode` get exact per-node accumulated cost; stream-captured graphs (`cuStreamBeginCapture`) currently use a flat `32`-token weight per replay.
>
> Architecture sequence diagrams are in [`docs/ARCHITECTURE.md`](https://github.com/motilayo/gpu-gatekeeper/blob/main/docs/ARCHITECTURE.md). Would love feedback from anyone running multi-tenant GPUs on Kubernetes!

---

## 3. Companion Technical Blog Post

### Title: Why Kubernetes DRA Needs an eBPF Data Plane (And Why `LD_PRELOAD` and `/dev/nvidia*` `ioctl` Interception Fall Short)

#### 1. The Control Plane / Data Plane Gap in Kubernetes 1.34 DRA
Kubernetes 1.34 promoted **Dynamic Resource Allocation (`resource.k8s.io/v1`)** to GA. DRA replaces Kubernetes' 10-year-old integer device plugin counter (`nvidia.com/gpu: 1`) with structured `DeviceClass`, `ResourceClaim`, and `ResourceSlice` objects. For the first time, the Kubernetes scheduler can natively express fractional GPU requests like:
- *"Give Pod A `20%` of GPU 0's compute and `4 GiB` of VRAM."*
- *"Give Pod B `80%` of GPU 0's compute and `12 GiB` of VRAM."*

**But there is a catch:** DRA is strictly a **control-plane scheduling and CDI wiring API**. Once Kubelet calls `NodePrepareResources` and mounts `/dev/nvidia0` into both containers via Container Device Interface (CDI), Kubernetes steps out of the way. Unless you have an A100/H100 with hardware MIG partitions, **nothing in the kernel enforces that Pod A actually stays within 20% compute or 4 GiB of VRAM.**

#### 2. Why Not Intercept `/dev/nvidiactl` `ioctl` Calls in the Kernel (`kprobes`)?
A common first instinct for kernel engineers is: *"Why not attach eBPF `kprobes` or LSM hooks to `nvidia_ioctl` in `nvidia.ko`?"*
On modern NVIDIA GPUs (Turing through Hopper/Blackwell), **kernel Launches do not go through `ioctl`!**
- During context initialization, the CUDA driver maps a user-space command ring (pushbuffer) and doorbell register directly into the process's virtual address space (`nvidia-uvm` / MMIO doorbell).
- When `cuLaunchKernel` or `cuGraphLaunch` runs, the user-space driver (`libcuda.so.1`) writes the launch descriptor directly to user-mapped memory and rings the GPU doorbell—**bypassing the OS kernel (`ioctl`) entirely** on the hot path.
- Therefore, the lowest layer where every kernel launch and stream-ordered allocation (`cuMemAllocAsync`) is guaranteed to be visible with semantic parameters (`gridDim`, `blockDim`, `bytesize`, `CUgraphExec`) is the **CUDA Driver API boundary inside `libcuda.so.1`**.

#### 3. Why `uprobes` on Host `libcuda.so.1` Beat `LD_PRELOAD` (`libvgpu.so`)
Existing fractional GPU projects (such as CNCF HAMi and Tencent `vcuda`) intercept `libcuda.so.1` by injecting a custom C shared library into every container using `LD_PRELOAD` or `/etc/ld.so.preload`.
Moving that interception out of the container and into **host kernel eBPF `uprobes`/`uretprobes`** changes three fundamental properties:
1. **Zero Container Mutation:** You never touch the pod spec, volume mounts, or `LD_PRELOAD`. Distroless images and custom container entrypoints work untouched.
2. **Unbypassable Host Isolation:** A process inside a container can `unset LD_PRELOAD` or corrupt a user-space shared-memory lockfile. It cannot detach an eBPF program owned by root in the host kernel, nor can it forge `bpf_get_current_cgroup_id()`.
3. **Byte-Exact Crash Cleanup & Pre-OOM `SIGKILL`:** By correlating `uprobe/cuMemAlloc_v2` + `uretprobe/cuMemAlloc_v2` with `uprobe/cuMemFree_v2` inside BPF hash maps, the host kernel knows the exact live VRAM footprint of every `cgroup v2` inode and can deliver `bpf_send_signal(SIGKILL)` inside the offending allocation call before a runaway container takes down its neighbors.


---

## 4. Reddit Launch Posts (`r/kubernetes` & `r/LocalLLaMA`)

### A. `r/kubernetes` (Post as a **Text Post** — `r/kubernetes` strongly prefers technical self-posts over bare link drops)

**Title:**
`I built an out-of-band eBPF GPU QoS governor & Kubernetes 1.34 DRA driver for sharing non-MIG GPUs (T4, L4, A10G, RTX) without LD_PRELOAD`

**Body:**
```markdown
Repo (Apache-2.0): https://github.com/motilayo/gpu-gatekeeper

If you run A100s or H100s and only need up to 7 static slices, hardware MIG is great.
If you run inference, notebooks, or batch jobs on non-MIG GPUs (`T4`, `L4`, `L40S`, `A10G`, or consumer RTX cards in homelabs), sharing a single GPU across pods in Kubernetes usually means picking between two painful options:

1. **NVIDIA Time-Slicing / MPS:** Zero per-pod compute QoS and zero per-pod VRAM isolation. A noisy neighbor running batch prefill or a runaway `torch.empty()` floods the GPU hardware queue (spiking p99 latency for co-located pods) or triggers a device-wide CUDA OOM that kills your production pod. Also, `nvidia-smi` / DCGM only sees host PIDs or whole-GPU metrics—not per-cgroup / per-`ResourceClaim` kernel launch rates.
2. **`LD_PRELOAD` shims (HAMi-core, vcuda):** Works by injecting a custom `.so` into every container via a mutating webhook and overriding `cuLaunchKernel` / `cuMemAlloc` inside the container's own process. That mutates pod specs, can break distroless images or newer CUDA 12.8 symbols (`cuLaunchKernelEx`, `cuGraphLaunch`), and runs inside the unprivileged container (`unset LD_PRELOAD`).

### What I built: G-Gate (`v0.5.1`)
G-Gate moves GPU attribution, compute rate-limiting, and VRAM enforcement **out of the container and into the host Linux kernel** via eBPF `uprobes`/`uretprobes` attached to the host's `libcuda.so.1`, wired into **Kubernetes 1.34 Dynamic Resource Allocation (`resource.k8s.io/v1`)**:

- **Zero Container Injection:** Containers are not mutated (`LD_PRELOAD` is never set).
- **Per-Pod & Per-`ResourceClaim` Attribution:** Every `cuLaunchKernel`, `cuLaunchKernelEx`, `cuGraphLaunch`, `cuMemAlloc_v2`, `cuMemAllocAsync`, `cuMemFree_v2`, and `cuMemFreeAsync` is attributed to its `cgroup v2` inode (`bpf_get_current_cgroup_id()`), resolved against Kubelet's `:10250/pods` API + DRA `ResourceClaim` objects, and exported on `:9090/metrics` (plus a built-in `:9090/` live web console).
- **In-Kernel Token-Bucket QoS (`SIGSTOP`/`SIGCONT` micro-slicing):** Before a low-priority pod can flood the GPU's hardware command queue, G-Gate charges `(gridDim * blockDim) >> 10` tokens per launch (or accumulated multi-node cost for `cuGraphLaunch`) and micro-pauses over-budget processes in `<=50 ms` slices with debt carry.
- **Byte-Exact VRAM Tracking & Pre-OOM `SIGKILL` (`--vram-policy=audit|throttle|kill`):** Because `cuMemFree_v2(CUdeviceptr dptr)` only passes the GPU address and not the byte size, G-Gate caches the output pointer in `uprobe/cuMemAlloc_v2`, dereferences the GPU address in `uretprobe/cuMemAlloc_v2`, stores `addr -> {cgroup_id, bytesize}` in a BPF map, and decrements the owning cgroup on free. In `kill` mode, a pod that breaches its VRAM limit gets an immediate in-kernel `bpf_send_signal(SIGKILL)` before it can OOM its neighbors.
- **Native K8s 1.34 DRA (`resource.k8s.io/v1` + `v1beta1`) & Pod Annotations:** Supports both DRA `ResourceClaimTemplate` + CDI `0.6.0` specs + `ResourceSlice` publishing AND simple Pod annotations (`g-gate.io/tokens-per-sec`, `g-gate.io/max-vram-mb`, `g-gate.io/vram-policy`).

### Real-Hardware Benchmarks on an AWS Spot Tesla T4 (PyTorch 2.7 + CUDA 12.8)
All raw CSVs, JSON latency distributions, and the `<$0.05` Spot reproduction script are in [`benchmarks/`](https://github.com/motilayo/gpu-gatekeeper/blob/main/benchmarks/VALIDATION_REPORT.md):

- **Single-launch uprobe overhead:** `+2.40 µs` median (`65.03 µs` native vs `67.43 µs` with G-Gate at 15.2k launches/sec).
- **20/80 Priority Split (PyTorch GEMMs):** `1,250.81` vs `5,024.81` IOPS (`0.24893` ratio vs `0.25000` target = **99.57% precision**). High-priority `tenant-80` ran at **56.7 µs median launch latency** (below the 65.03 µs single-tenant baseline) while `tenant-20` absorbed the queueing delay (`952.6 µs`).
- **10 / 30 / 60 3-Tenant Split:** `10.005% : 30.016% : 59.980%` (**99.92% precision**, CV `<0.022%`).
- **CUDA Graph Replay (`torch.cuda.CUDAGraph` via `uprobe/cuGraphLaunch`):** `196.71` vs `789.63` replays/sec (**99.65% precision**).
- **VRAM Lifecycle:** `258.00 MiB` tensor freed via `torch.cuda.empty_cache()` -> exact `270,532,608 B` (`258.000000 MiB`) reclaimed with **0 B drift**.
- **Batch Prefill Flood (`16x512`) vs. Interactive LLM Decode (`Qwen2.5-0.5B`):** Clamped the noisy neighbor's `8,192`-token prefill flood by **96.6%** (`7,435 -> 253.7 tok/s`) with `0` throttles on the interactive decoder.

### Honest Limitations (Where It Hurts)
1. **Eager-Mode Python LLM Decoding vs. CUDA Graphs:** When we tested uncompiled eager-mode Hugging Face `transformers` (`Qwen2.5-0.5B` without `torch.compile` or CUDA Graphs), we found it launches **~1,640 tiny CUDA kernels per generated token**. At `~3.5 µs` per Linux `uprobe` breakpoint trap, `1,640 * 3.5 µs = ~5.7 ms/token` (~16% throughput tax even with 0 throttles). We added a surplus token-bucket fast-path in eBPF to trim in-kernel math, but the fundamental takeaway is: for LLM decoding, use engines that capture decode steps into **CUDA Graphs** (`vLLM`, `TensorRT-LLM`, `torch.cuda.CUDAGraph`), which collapse 1,640 traps into **1 `uprobe/cuGraphLaunch` trap per token (`3.5 µs`)**.
2. **Submission-Time Pacing Only:** G-Gate paces launches *before* they enter the GPU hardware queue; it cannot preempt a kernel already executing on SMs.
3. **Privileged DaemonSet:** Requires `CAP_BPF`, `CAP_KILL`, and `hostPID: true` on GPU nodes (see `SECURITY.md`).

Architecture sequence diagrams: https://github.com/motilayo/gpu-gatekeeper/blob/main/docs/ARCHITECTURE.md
Feedback, critiques, and bug reports are very welcome!
```

---

### B. `r/LocalLLaMA` (Tailored for Multi-Tenant GPU / Homelab / Inference Engineers)

**Title:**
`We tested an eBPF GPU governor on a Tesla T4 to stop batch prefill floods from wrecking interactive LLM decode (and learned a painful lesson about eager-mode PyTorch)`

**Body:**
```markdown
Repo & raw T4 data (Apache-2.0): https://github.com/motilayo/gpu-gatekeeper

If you share a single non-MIG GPU (`T4`, `L4`, `3090`, `4090`, `A10G`) between an interactive chat/coding model and a background job (RAG indexing, batch summarization, embeddings, or a second user), two bad things usually happen:
1. A heavy batch prefill floods the GPU's hardware command queue and spikes inter-token latency (ITL) for the interactive stream.
2. One container allocates too much VRAM and triggers a CUDA OOM that crashes the other container.

I built **G-Gate**, an out-of-band Linux eBPF daemon that attaches `uprobes`/`uretprobes` to the host's `libcuda.so.1` (zero `LD_PRELOAD` or container changes) to enforce per-cgroup / per-Pod compute token-bucket quotas and hard VRAM ceilings (`--vram-policy=audit|throttle|kill`).

### What Worked Great on a Physical Tesla T4 (CUDA 12.8 + PyTorch 2.7)
1. **Byte-Exact Per-Container VRAM Tracking & OOM Kill (`0%` speed penalty):**
   In the CUDA Driver API, `cuMemFree_v2(dptr)` only passes the 64-bit GPU pointer, not the size. By caching the output pointer on `uprobe/cuMemAlloc_v2` and dereferencing the GPU address on `uretprobe/cuMemAlloc_v2`, G-Gate tracks live VRAM per container to the exact byte (`258.00 MiB` freed via `torch.cuda.empty_cache()` -> `270,532,608 B` reclaimed, `0 B` drift) and can `SIGKILL` an over-quota process inside `cuMemAlloc` before it OOMs your main model.
2. **CUDA Graph Replay (`uprobe/cuGraphLaunch`) & GEMM Pacing (`99.57%–99.98%` precision):**
   Under a `20/80` split, high-priority `tenant-80` kept a **56.7 µs median launch latency** (faster than the 65.03 µs single-tenant baseline) while `tenant-20` was micro-paused in user space (`952.6 µs`). On `torch.cuda.CUDAGraph` replays, the `20/80` split hit `196.71` vs `789.63` replays/sec (**99.65% accuracy**).
3. **Clamping a `16x512` (`8,192`-token) Batch Prefill Flood:**
   Because G-Gate weights kernel launches by `(gridDim * blockDim) >> 10`, when we ran a continuous `batch=16, seq=512` prefill flood (`7,435 tok/s` unmanaged) next to a `batch=1` interactive `Qwen2.5-0.5B` decoder, G-Gate automatically penalized the huge prefill grids—clamping the prefill flood by **96.6%** (`7,435 -> 253.7 tok/s`) with **0 throttles** on the interactive decoder.

### The Painful Lesson: Eager-Mode `transformers` vs. CUDA Graphs
Before running the LLM benchmark, we pre-registered our predictions in the repo (`benchmarks/results/llm/PREDICTIONS.md`). Here is the catch we found:
- A single Linux eBPF `uprobe` trap (`int3` -> kernel -> user) costs **~2.4 to 3.5 µs**.
- When you run **uncompiled eager-mode Hugging Face `transformers` (`model.generate()` without CUDA Graphs)**, a 24-layer `Qwen2.5-0.5B` model dispatches **~1,640 tiny CUDA kernels for every single generated token**.
- `1,640 launches * 3.5 µs = ~5.7 ms per generated token` (~16% slowdown even with `0` throttles!).
- **Why CUDA Graphs (`vLLM`, `llama.cpp` graph mode, `torch.cuda.CUDAGraph`) fix this:** When the decode step is captured into a CUDA Graph, those 1,640 kernel launches collapse into **1 `cuGraphLaunch` call per token**, dropping the eBPF trap overhead from `5.7 ms/token` down to **`3.5 µs/token` (`<0.1%`)**.

Full report, raw CSV/JSON telemetry, and architecture diagrams are in the repo:
- Repo: https://github.com/motilayo/gpu-gatekeeper
- LLM Decode & Prefill Flood Report: https://github.com/motilayo/gpu-gatekeeper/blob/main/benchmarks/results/llm/SUMMARY.md
- Architecture Diagrams: https://github.com/motilayo/gpu-gatekeeper/blob/main/docs/ARCHITECTURE.md
```
