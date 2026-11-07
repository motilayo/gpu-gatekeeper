# Security Policy

## Supported Versions

Only the latest minor release branch receives security updates and vulnerability patches.

| Version | Supported          |
| ------- | ------------------ |
| 0.5.x   | :white_check_mark: |
| < 0.5.0 | :x:                |

---

## Reporting a Vulnerability

If you discover a security vulnerability in G-Gate, please disclose it responsibly. Do **not** open a public issue.

- **GitHub Private Advisory:** Please report the vulnerability through GitHub's [Private Vulnerability Reporting](https://github.com/motilayo/gpu-gatekeeper/security/advisories/new).
- **Direct Contact:** If you cannot use GitHub Security Advisories, contact the project maintainer via email at `joshuaagboola@live.ca` with subject `[SECURITY] G-Gate Vulnerability Report`.

Please include:
1. Detailed description of the vulnerability and attack vector.
2. Step-by-step reproduction instructions or proof-of-concept code.
3. Impact assessment on Kubernetes nodes or multi-tenant GPU workloads.

We acknowledge vulnerability submissions within 48 hours and work with reporters on patch verification and coordinated disclosure.

---

## Privilege Model & Node Attack Surface

G-Gate operates as an out-of-band kernel observability and Quality-of-Service (QoS) governor. To monitor GPU driver interactions and enforce token-bucket throttling, the `ggate-agent` DaemonSet requires specific host-level privileges:

### 1. Linux Capabilities & Kernel Access
- **`CAP_BPF` / `CAP_SYS_ADMIN` (or root):** Required to load eBPF programs (`BPF_PROG_TYPE_KPROBE` / `uprobe`), allocate BPF hash maps and ringbuffers, and attach uprobes to user-space dynamic libraries (`libcuda.so.1`).
- **`CAP_KILL`:** Required for the QoS governor to send POSIX signals to workload processes:
  - `SIGSTOP` and `SIGCONT`: Used for nanosecond token-bucket micro-pausing (up to `MAX_PAUSE_NS = 50 ms`).
  - `SIGKILL`: Dispatched when active VRAM enforcement (`--vram-policy=kill` or `g-gate.io/vram-policy: "kill"`) is explicitly enabled and a workload exceeds its memory limit.
- **`hostPID: true`:** Required because the agent runs inside a container but needs to inspect and signal workload process PIDs running in container cgroups across the host PID namespace.

### 2. Filesystem Mounts
- **`/sys/fs/cgroup` (read-only):** Required to resolve `cgroup v2` directory hierarchy, detect Pod/container inode numbers, and correlate processes to Kubernetes workloads.
- **NVIDIA Driver Libraries (`libcuda.so.1`):** Required to attach uprobes to CUDA runtime API symbols (`cuLaunchKernel`, `cuGraphLaunch`, `cuMemAlloc_v2`, etc.). Can be mounted from `/run/nvidia/driver`, `/usr/lib64`, or `/proc/1/root`.

### 3. Kubernetes RBAC & Cluster Permissions
The agent requires a cluster `ServiceAccount` with:
- **`nodes`, `nodes/proxy`, `pods` (verbs: `["get", "list", "watch"]` in `apiGroups: [""]`):** Used to query node-local Kubelet HTTPS endpoints (`https://127.0.0.1:10250/pods`) and resolve Pod metadata, container IDs, and QoS annotations.
- **`resourceclaims`, `resourceclaimtemplates`, `deviceclasses` (verbs: `["get", "list", "watch"]` in `apiGroups: ["resource.k8s.io"]`):** Used by the DRA Claim Controller to discover bound Dynamic Resource Allocation claims.
- **`resourceslices` (verbs: `["get", "list", "watch", "create", "update", "patch", "delete"]` in `apiGroups: ["resource.k8s.io"]`):** Used by the Node `ResourceSlice` publisher to publish local GPU capacity to the Kubernetes cluster scheduler.

### 4. Active Enforcement Guardrails
> [!WARNING]
> When configured with `--vram-policy=kill` (or via Pod annotation `g-gate.io/vram-policy: "kill"` / DRA parameter `vramPolicy: "kill"`), G-Gate executes immediate kernel-level process termination via `bpf_send_signal(SIGKILL)` upon detecting any memory allocation that exceeds the configured `max_vram_bytes`. This policy is designed for strict multitenancy isolation but terminates workloads immediately. Use `audit` (default) or `throttle` if abrupt process termination is undesirable.
