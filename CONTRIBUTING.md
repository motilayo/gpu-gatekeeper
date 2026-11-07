# Contributing to G-Gate

Thank you for your interest in contributing to G-Gate! We welcome bug reports, documentation improvements, feature additions, and performance optimizations.

---

## Development Environment Setup

G-Gate uses Linux eBPF and cgroup v2. On macOS (Apple Silicon or Intel), we provide a fully automated, $0 local development environment powered by [Lima](https://github.com/lima-vm/lima):

### 1. Install Prerequisites
- macOS: `brew install lima`
- Linux: native kernel 5.8+ with BTF support (`/sys/kernel/btf/vmlinux`), `clang`, `llvm`, `libbpf-dev`, `bpftool`, and `golang-go 1.24+`.

### 2. Boot the Development VM
```bash
# Start the pre-configured Ubuntu 24.04 VM with eBPF tooling
limactl start lima.yaml
```

The `Makefile` automatically detects macOS and routes build commands directly into the Lima VM (`limactl shell ggate`).

### 3. Build & Test Targets
```bash
# Compile mock CUDA driver and workload simulator
make build-mock

# Run bpf2go eBPF code generation (multi-arch arm64/x86_64)
make generate

# Compile ggate-agent binary
make build-agent

# Execute Go unit test suite
make test-unit

# Run full automated end-to-end acceptance suite (all 13 milestones)
make test-e2e

# Run live k3s integration test suite (requires root inside Linux)
make test-k3s
```

---

## Pull Request Guidelines

Before submitting a pull request, ensure:
1. **Tests Pass:** All unit tests (`make test-unit`) and acceptance tests (`make test-e2e`) pass cleanly with zero errors.
2. **Code Hygiene:** Code is formatted with `gofmt` and linted without warnings (`helm lint deploy/helm/ggate`).
3. **Dependencies:** Do not add unnecessary transitive dependencies. G-Gate maintains a strict zero-vulnerability security posture.
4. **Hardware Validation Claims:** Any performance, precision, or attribution claims involving physical GPUs must include raw reproducible telemetry logs (CSV, Prometheus scrapings, `environment.txt`) under `benchmarks/results/` following the schema documented in `benchmarks/VALIDATION_REPORT.md`.
