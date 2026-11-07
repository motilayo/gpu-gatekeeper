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
