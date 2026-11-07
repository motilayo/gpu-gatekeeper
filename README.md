# G-Gate v2: eBPF GPU Attribution & QoS Governor

G-Gate is an out-of-band, zero-injection GPU observability and Quality-of-Service (QoS) governor for Linux and Kubernetes. It intercepts user-space GPU driver API calls via eBPF `uprobes`, attributes GPU compute launches and active VRAM allocations to `cgroup v2` inodes and Kubernetes Pods/Containers, exports per-tenant Prometheus metrics, and enforces compute quotas via nanosecond token-bucket rate limiting and `SIGSTOP`/`SIGCONT` micro-pausing.

---

### Verified Real-Hardware Benchmark (NVIDIA Tesla T4 + PyTorch `2.7.0+cu128`)

Validated on AWS EC2 `g4dn.xlarge` Spot (`us-east-2`, `Tesla T4`, `/lib/x86_64-linux-gnu/libcuda.so.1`) running two concurrent PyTorch `torch.matmul` workloads (`10.0s` duration) in separate `cgroup v2` slices with a 20% / 80% token-bucket split:

| Tenant Slice | Configured Quota | Achieved PyTorch MatMul IOPS | Intercepted CUDA Launches (`ggate_gpu_launches_total`) | Attributed Active VRAM (`ggate_vram_allocated_bytes`) | Cumulative QoS Pause (`ggate_gpu_throttled_seconds_total`) |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **`ggate-pytorch-20`** (20%) | `100,000` tokens/sec | **`1,242.47` matmuls/sec** (`12,425` iters) | `24,856` | `300,024,832 B` (`~286.1 MiB`) | `2.14969 s` (`6,748` throttles) |
| **`ggate-pytorch-80`** (80%) | `400,000` tokens/sec | **`4,997.14` matmuls/sec** (`49,972` iters) | `99,950` | `300,024,832 B` (`~286.1 MiB`) | `0.48380 s` (`6,631` throttles) |
| **20% / 80% Ratio** | **Target: `0.2500`** | **Achieved: `0.2486` (99.44% accuracy)** | **Ratio: `0.2487`** | **Zero `LD_PRELOAD` / Zero Crashes** | **Out-of-Band eBPF `uprobe`** |

#### Token-Bucket Physics & Self-Clocking Scheduler Analysis

1. **Deterministic Micro-Slicing (`MAX_PAUSE_NS` 10 ms Ceiling):** Rather than blocking a process for seconds when encountering large token deficits, G-Gate enforces a 10 ms micro-pause ceiling (`MAX_PAUSE_NS = 10,000,000 ns`). Any remaining deficit is recorded in `debt_tokens` and paid off in subsequent micro-slices, preventing heartbeat timeouts or healthcheck dropouts in Kubernetes.
2. **Deficit Payoff Math & Future Timestamp Guard:** When a process is paused for `sleep_ns`, the token bucket's `last_update_ns` is advanced to `max(now, last_update_ns) + sleep_ns`. Refills only occur when `now > last_update_ns`, preventing paused threads from accumulating "free" tokens during their penalty sleep.
3. **True Wall-Clock Latency Accounting:** The userspace Go ringbuffer reader records `pausedAt := time.Now()` upon receiving a `throttle_event` and measures `time.Since(pausedAt)` when dispatching `SIGCONT`, ensuring `ggate_gpu_throttled_seconds_total` reflects physical wall-clock pause duration without under-counting.
4. **CUDA Graph Replay & Async Memory Lifecycle:** G-Gate intercepts `cuGraphLaunch` (attributing fixed-cost token weight to graph execution without kernel disassembly) and tracks modern stream-ordered VRAM lifecycle via `cuMemAllocAsync` / `cuMemFreeAsync`.
5. **Zero-Config Driver Auto-Discovery (`--libcuda=auto`):** Automatically discovers `libcuda.so.1` across canonical distro paths, NVIDIA GPU Operator containers (`/run/nvidia/driver/...`), cloud images, and `/proc/1/root/...`.

---

## Architecture

```
+-----------------------------------------------------------------------------------------+
| User Space: Workload Process (PyTorch / CUDA inside Kubernetes Pod cgroup v2)           |
|   Calls cuLaunchKernel(), cuLaunchKernelEx(), cuGraphLaunch(),                          |
|         cuMemAlloc_v2(), cuMemAllocAsync(), cuMemFree_v2(), cuMemFreeAsync()            |
+---------------------------------------+-------------------------------------------------+
                                        | (eBPF uprobe / uretprobe)
                                        v
+-----------------------------------------------------------------------------------------+
| Kernel Space: eBPF Data Plane (bpf/ggate.bpf.c)                                         |
|   1. Reads cgroup_id = bpf_get_current_cgroup_id()                                      |
|   2. Computes kernel launch token cost from gridDim & blockDim (or default graph weight)|
|   3. Updates cgroup_quota_map (BPF_MAP_TYPE_HASH) token bucket with 10ms micro-pause   |
|   4. If tokens < cost: emits throttle_event, increments pause ns, sends SIGSTOP         |
|   5. Tracks live VRAM lifecycle (cuMemAlloc_v2 / cuMemAllocAsync & frees)               |
|   6. Flags VRAM quota overages against max_vram_bytes                                   |
+---------------------------------------+-------------------------------------------------+
                                        | (events_ringbuf: BPF_MAP_TYPE_RINGBUF)
                                        v
+-----------------------------------------------------------------------------------------+
| User Space: G-Gate Go Control Plane (cmd/ggate-agent)                                   |
|   1. K8s Pod Resolver: maps cgroups to namespace, pod, container via HTTPS Kubelet      |
|   2. Annotation Quota Controller: auto-enrolls tokens-per-sec & max-vram-mb             |
|   3. Dynamic Cgroup Watcher: syncs path/glob quotas to cgroup inodes                    |
|   4. RingBuffer Consumer & Wall-Clock Pacer: resumes tasks via time.AfterFunc + SIGCONT |
|   5. Prometheus Exporter: exposes /metrics with Pod labels on :9090                     |
+-----------------------------------------------------------------------------------------+
```

---

## Features

- **Zero-Injection Observability:** Intercepts `cuLaunchKernel`, `cuLaunchKernelEx`, `cuGraphLaunch`, `cuMemAlloc_v2`, `cuMemAllocAsync`, `cuMemFree_v2`, and `cuMemFreeAsync` without modifying container images or injecting `LD_PRELOAD`.
- **Driver Auto-Discovery (`--libcuda=auto`):** Automatically locates active NVIDIA driver libraries across container runtimes, GPU Operator paths, and host filesystems.
- **Kubernetes Pod Label Resolution:** Correlates cgroup v2 inodes with Kubelet HTTPS (`:10250/pods`) using ServiceAccount Bearer token authentication, enriching Prometheus metrics with `namespace`, `pod`, and `container` labels.
- **Pod Annotation Quotas:** Declare GPU compute and VRAM quotas directly on Pod manifests using `g-gate.io/tokens-per-sec` and `g-gate.io/max-vram-mb` annotations with zero agent restarts.
- **10 ms Micro-Pausing QoS Governor:** Nanosecond integer token-bucket rate limiter that throttles workloads via `SIGSTOP`/`SIGCONT` micro-pausing with a 10 ms ceiling to prevent process starvation or Kubernetes probe timeouts.
- **True Wall-Clock Throttling Tracking:** Exports `ggate_gpu_throttled_seconds_total` measuring physical pause time added by QoS rate limiting.
- **Live VRAM Lifecycle Tracking:** Accurately attributes memory allocations on `cuMemAlloc_v2` / `cuMemAllocAsync` and releases on `cuMemFree_v2` / `cuMemFreeAsync` back to 0, flagging quota overages in `ggate_vram_quota_exceeded_total`.
- **Production Packaging:** Multi-stage `Dockerfile`, Helm chart (`deploy/helm/ggate`), GitHub Actions E2E CI (`.github/workflows/ci.yml`), and pre-built Grafana dashboard (`deploy/grafana/ggate-dashboard.json`).
- **$0 Local Mock Development:** Full multi-architecture development on Apple Silicon macOS via Lima VM without requiring physical NVIDIA GPUs.

---

## Quickstart (Lima VM on Apple Silicon)

### Prerequisites

- macOS with Apple Silicon (or native Linux with eBPF support)
- [Lima](https://github.com/lima-vm/lima) (`brew install lima`)

### 1. Boot the Development VM

```bash
limactl start lima.yaml
```

The VM provisions Ubuntu 24.04 with `clang`, `llvm`, `bpftool`, `vmlinux.h`, and `golang-go`.

### 2. Run Automated End-to-End Acceptance Tests

The Makefile automatically routes commands into the Lima VM when invoked on macOS:

```bash
make test-e2e
```

To run the live k3s integration test (requires root / k3s inside Linux):

```bash
make test-k3s
```

This automated test suite verifies all milestones:
1. **Milestone 1:** Compiles `mock/libcuda.so` and verifies symbol exports for `cuLaunchKernel`, `cuLaunchKernelEx`, `cuGraphLaunch`, `cuMemAlloc_v2`, `cuMemAllocAsync`, `cuMemFree_v2`, and `cuMemFreeAsync`.
2. **Milestone 2:** Runs `workload_sim -n 5000 -vram 104857600` and validates Prometheus attribution metrics for the cgroup inode.
3. **Milestone 3:** Configures dynamic 20% / 80% quota paths, starts the agent, creates the cgroups *after* agent startup, runs concurrent workloads, and verifies the 20% slice achieves 200.0 IOPS (±5%) and a 0.25 ratio (±5%).
4. **Milestone 4:** Runs `workload_sim -free-on-exit` and confirms allocated VRAM is accurately decremented back to 0.
5. **Milestone 5:** Connects agent to mock Kubelet Pod metadata with `g-gate.io/tokens-per-sec="6400"` and `g-gate.io/max-vram-mb="32"`, verifies automatic quota discovery, and confirms enriched Pod labels, `ggate_gpu_throttled_seconds_total > 0`, and VRAM quota violation tracking.
6. **Milestone 9 (v0.2):** Validates CUDA Graphs (`cuGraphLaunch`), stream-ordered memory (`cuMemAllocAsync` / `cuMemFreeAsync`), `--libcuda auto` resolution, and micro-sliced latency tracking.

### Build Targets

```bash
make build-mock    # Compiles build/libcuda.so and build/workload_sim
make generate      # Runs multi-arch bpf2go code generation
make build-agent   # Builds build/ggate-agent (depends on generate)
make test-unit     # Runs Go unit tests
make test-e2e      # Runs full E2E acceptance test suite
make test-k3s      # Runs live k3s Kubernetes test suite
make clean         # Removes build artifacts
```

---

## Kubernetes Pod Annotation Quotas

Annotate any Kubernetes Pod to enforce GPU token-bucket quotas and VRAM limits without configuring the agent:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: llama3-inference-7b
  namespace: ai-prod
  annotations:
    g-gate.io/tokens-per-sec: "6400"  # ~200 kernel launches/sec for 128x256 grid
    g-gate.io/burst-tokens: "64"      # Bucket burst capacity
    g-gate.io/max-vram-mb: "16384"    # 16 GiB VRAM quota (tracks violations)
spec:
  containers:
    - name: pytorch
      image: pytorch/pytorch:2.2.0-cuda12.1-cudnn8-runtime
      command: ["python", "inference.py"]
```

---

## CLI Reference

### `ggate-agent`

```bash
./build/ggate-agent [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--libcuda` | `./build/libcuda.so` | Path to `libcuda.so` library, mock, or `auto` for driver discovery |
| `--metrics-addr` | `:9090` | HTTP address for Prometheus metrics |
| `--quota` | *(repeatable)* | Manual quota specification in format `<target>:<refill_rate>:<max_burst>` |
| `--kubelet-pods-url` | `""` | HTTP/HTTPS URL returning Kubelet `/pods` JSON |
| `--kubelet-pods-file` | `""` | Path to local JSON file with Kubelet `/pods` format |
| `--kubelet-token-file` | `"/var/run/secrets/kubernetes.io/serviceaccount/token"` | Path to ServiceAccount bearer token for Kubelet HTTPS `:10250/pods` |

The `<target>` for `--quota` can be:
- A numeric cgroup inode: `--quota 12345:6400:64`
- An absolute or relative cgroup path: `--quota /sys/fs/cgroup/ggate-test:6400:64`
- A glob pattern: `--quota "kubepods.slice/kubepods-burstable.slice/*.scope:6400:64"`

### `workload_sim`

```bash
./build/workload_sim [flags]
```

| Flag | Default | Description |
|---|---|---|
| `-n` | `10000` | Number of kernel launch iterations (`0` = run for duration) |
| `-duration` | `0` | Run time in seconds (if `> 0`, runs until time elapses) |
| `-vram` | `67108864` | VRAM bytes to allocate via `cuMemAlloc_v2` / `cuMemAllocAsync` (64 MiB) |
| `-grid` | `128` | Grid dimension X |
| `-block` | `256` | Block dimension X |
| `-graph-launch` | `0` | Replays kernel graph via `cuGraphLaunch` instead of `cuLaunchKernel` |
| `-async-vram` | `0` | Uses `cuMemAllocAsync` / `cuMemFreeAsync` stream-ordered memory |
| `-free-on-exit` | `0` | Calls `cuMemFree_v2` or `cuMemFreeAsync` before exiting |

---

## Cloud GPU Spot Smoke-Testing (`deploy/cloud/`)

Validate G-Gate against physical NVIDIA GPUs and real PyTorch workloads on low-cost Cloud Spot instances ($0.15 - $0.22/hr):

### AWS EC2 Spot (`g4dn.xlarge` / `g5.xlarge`)

```bash
# Provision Spot instance with 90-minute auto-terminate dead-man's switch
./deploy/cloud/aws-spot-smoke.sh up

# Run real GPU verification suite (PyTorch matrix multiplications)
./deploy/cloud/aws-spot-smoke.sh run-verify

# Optional: SSH into the GPU instance
./deploy/cloud/aws-spot-smoke.sh ssh

# Teardown instance and security group
./deploy/cloud/aws-spot-smoke.sh teardown
```

### GCP Cloud Spot (`g2-standard-4`)

```bash
# Provision Spot instance with GCP-enforced --max-run-duration=90m
./deploy/cloud/gcp-spot-smoke.sh up
./deploy/cloud/gcp-spot-smoke.sh run-verify

# Optional: SSH into the instance
./deploy/cloud/gcp-spot-smoke.sh ssh

# Teardown instance
./deploy/cloud/gcp-spot-smoke.sh teardown
```

### What `verify-real-gpu.sh` Does:
1. Detects real NVIDIA GPU via `nvidia-smi -L` and validates kernel BTF at `/sys/kernel/btf/vmlinux`.
2. Locates the active `libcuda.so.1` path using `ldconfig`.
3. Auto-discovers Python environment with PyTorch.
4. Compiles `ggate-agent` on the target machine.
5. Spawns two isolated cgroups with 20% and 80% quotas (`ggate-pytorch-20` and `ggate-pytorch-80`).
6. Executes concurrent PyTorch matrix multiplications (`torch.matmul`) via `deploy/cloud/pytorch_bench.py` inside each cgroup.
7. Measures achieved PyTorch IOPS, computes ratio, and verifies Prometheus telemetry on `:9090/metrics`.
