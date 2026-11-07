# G-Gate v2: eBPF GPU Attribution & QoS Governor

G-Gate is an out-of-band, zero-injection GPU observability and Quality-of-Service (QoS) governor for Linux and Kubernetes. It intercepts user-space GPU driver API calls via eBPF `uprobes`, attributes GPU compute launches and active VRAM allocations to `cgroup v2` inodes and Kubernetes Pods/Containers, exports per-tenant Prometheus metrics, and enforces compute quotas via nanosecond token-bucket rate limiting and `SIGSTOP`/`SIGCONT` micro-pausing.

---

## Architecture

```
+-------------------------------------------------------------------------------+
| User Space: Workload Process (PyTorch / CUDA inside Kubernetes Pod cgroup v2) |
|   Calls cuLaunchKernel(), cuLaunchKernelEx(), cuMemAlloc_v2(), cuMemFree_v2() |
+---------------------------------------+---------------------------------------+
                                        | (eBPF uprobe / uretprobe)
                                        v
+-------------------------------------------------------------------------------+
| Kernel Space: eBPF Data Plane (bpf/ggate.bpf.c)                               |
|   1. Reads cgroup_id = bpf_get_current_cgroup_id()                            |
|   2. Computes kernel launch token cost from gridDim & blockDim                |
|   3. Updates cgroup_quota_map (BPF_MAP_TYPE_HASH) token bucket                |
|   4. If tokens < cost: emits throttle_event, increments pause ns, sends 19    |
|   5. Tracks live VRAM lifecycle (cuMemAlloc_v2 + cuMemFree_v2)                |
|   6. Flags VRAM quota overages against max_vram_bytes                         |
+---------------------------------------+---------------------------------------+
                                        | (events_ringbuf: BPF_MAP_TYPE_RINGBUF)
                                        v
+-------------------------------------------------------------------------------+
| User Space: G-Gate Go Control Plane (cmd/ggate-agent)                         |
|   1. K8s Pod Resolver: maps cgroups to namespace, pod, container labels       |
|   2. Annotation Quota Controller: auto-enrolls tokens-per-sec & max-vram-mb   |
|   3. Dynamic Cgroup Watcher: syncs path/glob quotas to cgroup inodes          |
|   4. RingBuffer Consumer & Pacer: resumes tasks via time.AfterFunc + SIGCONT  |
|   5. Prometheus Exporter: exposes /metrics with Pod labels on :9090           |
+-------------------------------------------------------------------------------+
```

---

## Features

- **Zero-Injection Observability:** Intercepts `cuLaunchKernel`, `cuLaunchKernelEx`, `cuMemAlloc_v2`, and `cuMemFree_v2` without modifying container images or injecting `LD_PRELOAD`.
- **Kubernetes Pod Label Resolution:** Correlates cgroup v2 inodes with Kubelet Pod metadata, enriching Prometheus metrics with `namespace`, `pod`, and `container` labels.
- **Pod Annotation Quotas:** Declare GPU compute and VRAM quotas directly on Pod manifests using `g-gate.io/tokens-per-sec` and `g-gate.io/max-vram-mb` annotations with zero agent restarts.
- **Micro-Pausing QoS Governor:** Nanosecond integer token-bucket rate limiter that throttles workloads via `SIGSTOP`/`SIGCONT` micro-pausing with zero process crashes.
- **Throttling Duration Tracking:** Exports `ggate_gpu_throttled_seconds_total` to measure exact cumulative latency added by QoS rate limiting.
- **Live VRAM Lifecycle Tracking:** Accurately attributes memory allocations on `cuMemAlloc_v2` and releases on `cuMemFree_v2` back to 0, flagging quota overages in `ggate_vram_quota_exceeded_total`.
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

This automated test suite verifies all five milestones:
1. **Milestone 1:** Compiles `mock/libcuda.so` and verifies `cuLaunchKernel`, `cuLaunchKernelEx`, `cuMemAlloc_v2`, and `cuMemFree_v2` symbols.
2. **Milestone 2:** Runs `workload_sim -n 5000 -vram 104857600` and validates Prometheus attribution metrics for the cgroup inode.
3. **Milestone 3:** Configures dynamic 20% / 80% quota paths, starts the agent, creates the cgroups *after* agent startup, runs concurrent workloads, and verifies the 20% slice achieves 200.0 IOPS (±5%) and a 0.25 ratio (±5%).
4. **Milestone 4:** Runs `workload_sim -free-on-exit` and confirms allocated VRAM is accurately decremented back to 0.
5. **Milestone 5:** Connects agent to mock Kubelet Pod metadata with `g-gate.io/tokens-per-sec="6400"` and `g-gate.io/max-vram-mb="32"`, verifies automatic quota discovery, and confirms enriched Pod labels, `ggate_gpu_throttled_seconds_total > 0`, and VRAM quota violation tracking.

### Build Targets

```bash
make build-mock    # Compiles build/libcuda.so and build/workload_sim
make generate      # Runs multi-arch bpf2go code generation
make build-agent   # Builds build/ggate-agent (depends on generate)
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
| `--libcuda` | `./build/libcuda.so` | Path to `libcuda.so` library or mock |
| `--metrics-addr` | `:9090` | HTTP address for Prometheus metrics |
| `--quota` | *(repeatable)* | Manual quota specification in format `<target>:<refill_rate>:<max_burst>` |
| `--kubelet-pods-url` | `""` | HTTP URL returning Kubelet `/pods` JSON |
| `--kubelet-pods-file` | `""` | Path to local JSON file with Kubelet `/pods` format |

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
| `-vram` | `67108864` | VRAM bytes to allocate via `cuMemAlloc_v2` (64 MiB) |
| `-grid` | `128` | Grid dimension X |
| `-block` | `256` | Block dimension X |
| `-free-on-exit` | `0` | If set, calls `cuMemFree_v2` before exiting |

---

## Prometheus Metrics

Scrape endpoint: `http://localhost:9090/metrics`

All metrics include `cgroup_id`, `namespace`, `pod`, and `container` labels.

| Metric | Type | Description |
|---|---|---|
| `ggate_gpu_launches_total{cgroup_id, namespace, pod, container}` | Counter | Total GPU kernel launches intercepted |
| `ggate_gpu_throttled_total{cgroup_id, namespace, pod, container}` | Counter | Total kernel launches throttled via `SIGSTOP` |
| `ggate_gpu_throttled_seconds_total{cgroup_id, namespace, pod, container}` | Counter | Cumulative pause time enforced by QoS governor |
| `ggate_vram_allocated_bytes{cgroup_id, namespace, pod, container}` | Gauge | Current active allocated VRAM bytes |
| `ggate_vram_quota_bytes{cgroup_id, namespace, pod, container}` | Gauge | Configured maximum VRAM quota in bytes |
| `ggate_vram_quota_exceeded_total{cgroup_id, namespace, pod, container}` | Counter | Count of allocations exceeding configured VRAM quota |

---

## Kubernetes Deployment & Helm Chart

### Option A: Helm Chart (Recommended)

```bash
# Install or upgrade G-Gate via Helm
helm upgrade --install ggate ./deploy/helm/ggate -n kube-system --create-namespace
```

### Option B: Standalone DaemonSet Manifest

```bash
kubectl apply -f deploy/kubernetes/daemonset.yaml
```

The DaemonSet:
- Runs in `kube-system` namespace.
- Uses `hostPID: true` to issue `SIGCONT` resume signals to workload PIDs.
- Requires `BPF`, `PERFMON`, `SYS_ADMIN`, and `SYS_PTRACE` capabilities (no full container privilege needed).
- Mounts `/sys/fs/cgroup` and the host `libcuda.so.1` path.
- Includes standard Prometheus scrape annotations (`prometheus.io/scrape: "true"`).

---

## Grafana Dashboard

An out-of-the-box Grafana dashboard is provided in [`deploy/grafana/ggate-dashboard.json`](file:///Users/motilayo/workspace/gpu-gatekeeper/deploy/grafana/ggate-dashboard.json).

Panels included:
1. **GPU Kernel Launch Rate (IOPS):** `rate(ggate_gpu_launches_total[1m])` by pod.
2. **QoS Throttling Pause Rate:** `rate(ggate_gpu_throttled_seconds_total[1m])` in seconds paused per second.
3. **Active VRAM Allocated vs Quota:** `ggate_vram_allocated_bytes` compared against `ggate_vram_quota_bytes`.
4. **VRAM Quota Exceeded Violations:** `ggate_vram_quota_exceeded_total` counter.

To import:
1. Open Grafana UI -> **Dashboards** -> **New** -> **Import**.
2. Upload `deploy/grafana/ggate-dashboard.json`.
3. Select your Prometheus datasource.

---

## Cloud GPU Smoke Testing (AWS & GCP Spot < $0.50)

To validate G-Gate against real NVIDIA GPUs and genuine PyTorch workloads in the cloud, one-click spot smoke scripts are provided with automated **90-minute dead-man's safety switches** so instances never run indefinitely if forgotten.

### AWS Spot (`g4dn.xlarge` NVIDIA T4, ~$0.15/hr)

```bash
# Provision Spot instance and run real GPU verification suite
./deploy/cloud/aws-spot-smoke.sh up
./deploy/cloud/aws-spot-smoke.sh run-verify

# Optional: SSH into the instance
./deploy/cloud/aws-spot-smoke.sh ssh

# Teardown instance immediately (or let 90-minute auto-shutdown terminate it)
./deploy/cloud/aws-spot-smoke.sh teardown
```

### GCP Spot (`g2-standard-4` NVIDIA L4, ~$0.22/hr)

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
3. Compiles `ggate-agent` on the target machine.
4. Spawns two isolated cgroups with 20% and 80% quotas (`ggate-pytorch-20` and `ggate-pytorch-80`).
5. Executes concurrent PyTorch matrix multiplications (`torch.matmul`) via `deploy/cloud/pytorch_bench.py` inside each cgroup.
6. Measures achieved PyTorch IOPS and verifies Prometheus telemetry on `:9090/metrics`.
