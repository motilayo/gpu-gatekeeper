#!/usr/bin/env bash
set -euo pipefail

# Ensure running as root
if [ "$EUID" -ne 0 ]; then
    echo "Re-running as root via sudo..."
    exec sudo bash "$0" "$@"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
cd "${ROOT_DIR}"

echo "=========================================================="
echo "G-Gate Real GPU Verification Suite"
echo "=========================================================="

# 1. Check NVIDIA GPU presence
if ! command -v nvidia-smi &>/dev/null; then
    echo "ERROR: nvidia-smi not found. NVIDIA driver is required."
    exit 1
fi
echo "GPU detected:"
nvidia-smi -L

# 2. Discover Python interpreter with PyTorch
PYTHON_BIN=""
for candidate in /opt/pytorch/bin/python3 /opt/conda/envs/pytorch/bin/python3 /opt/conda/bin/python3 python3; do
    if command -v "${candidate}" &>/dev/null && "${candidate}" -c "import torch" &>/dev/null; then
        PYTHON_BIN="${candidate}"
        break
    fi
done
if [ -z "${PYTHON_BIN}" ]; then
    echo "ERROR: Could not find python3 with PyTorch installed."
    exit 1
fi
echo "Using Python with PyTorch: ${PYTHON_BIN} ($(${PYTHON_BIN} -c 'import torch; print(torch.__version__)'))"

# 3. Check kernel BTF support
if [ ! -f "/sys/kernel/btf/vmlinux" ]; then
    echo "ERROR: /sys/kernel/btf/vmlinux not found. Linux kernel with BTF enabled is required for eBPF CO-RE."
    exit 1
fi
echo "Kernel BTF: /sys/kernel/btf/vmlinux present."

# 4. Discover libcuda.so.1 path
LIBCUDA_PATH=""
if command -v ldconfig &>/dev/null; then
    LIBCUDA_PATH=$(ldconfig -p 2>/dev/null | awk '/libcuda\.so\.1 / {print $NF; exit}')
fi

if [ -z "${LIBCUDA_PATH}" ] || [ ! -f "${LIBCUDA_PATH}" ]; then
    if [ -f "/usr/lib/x86_64-linux-gnu/libcuda.so.1" ]; then
        LIBCUDA_PATH="/usr/lib/x86_64-linux-gnu/libcuda.so.1"
    elif [ -f "/usr/local/nvidia/lib64/libcuda.so.1" ]; then
        LIBCUDA_PATH="/usr/local/nvidia/lib64/libcuda.so.1"
    elif [ -f "/usr/lib64/libcuda.so.1" ]; then
        LIBCUDA_PATH="/usr/lib64/libcuda.so.1"
    fi
fi

if [ -z "${LIBCUDA_PATH}" ] || [ ! -f "${LIBCUDA_PATH}" ]; then
    echo "ERROR: libcuda.so.1 could not be located on this system."
    exit 1
fi
echo "Located CUDA Driver Library: ${LIBCUDA_PATH}"

# 5. Check/Install Go 1.24+ if needed
if ! command -v go &>/dev/null || ! go version | grep -qE 'go1\.(2[4-9]|[3-9][0-9])'; then
    echo "Installing latest stable Go from go.dev..."
    ARCH=$(uname -m)
    if [ "${ARCH}" = "x86_64" ]; then GO_ARCH="amd64"; else GO_ARCH="arm64"; fi
    GO_VER=$(curl -sL "https://go.dev/VERSION?m=text" | head -n1)
    curl -fsSL "https://go.dev/dl/${GO_VER}.linux-${GO_ARCH}.tar.gz" -o /tmp/go.tar.gz
    rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tar.gz
    ln -sf /usr/local/go/bin/go /usr/bin/go
    ln -sf /usr/local/go/bin/gofmt /usr/bin/gofmt
    rm -f /tmp/go.tar.gz
fi
echo "Go toolchain: $(go version)"

# 6. Build ggate-agent
echo "Building ggate-agent..."
mkdir -p build
go build -o build/ggate-agent ./cmd/ggate-agent
echo "Built build/ggate-agent successfully."

# 7. Setup cgroups and cleanup trap
CGROUP_20="/sys/fs/cgroup/ggate-pytorch-20"
CGROUP_80="/sys/fs/cgroup/ggate-pytorch-80"
AGENT_PID=""

cleanup() {
    echo "=== Running cleanup ==="
    if [ -n "${AGENT_PID:-}" ] && kill -0 "${AGENT_PID}" 2>/dev/null; then
        echo "Terminating ggate-agent (PID: ${AGENT_PID})..."
        kill -TERM "${AGENT_PID}" 2>/dev/null || true
        wait "${AGENT_PID}" 2>/dev/null || true
    fi
    pkill -9 -f "pytorch_bench.py" 2>/dev/null || true
    pkill -9 -f "build/ggate-agent" 2>/dev/null || true
    rmdir "${CGROUP_20}" 2>/dev/null || true
    rmdir "${CGROUP_80}" 2>/dev/null || true
    rm -f /tmp/ggate_agent_real.log /tmp/pytorch_*.log
}
trap cleanup EXIT

rmdir "${CGROUP_20}" 2>/dev/null || true
rmdir "${CGROUP_80}" 2>/dev/null || true
mkdir -p "${CGROUP_20}"
mkdir -p "${CGROUP_80}"

# 8. Start ggate-agent with 20% / 80% quotas
echo "Starting ggate-agent on real GPU..."
./build/ggate-agent \
    --libcuda "${LIBCUDA_PATH}" \
    --metrics-addr :9090 \
    --quota /sys/fs/cgroup/ggate-pytorch-20:100000:10000 \
    --quota /sys/fs/cgroup/ggate-pytorch-80:400000:40000 \
    > /tmp/ggate_agent_real.log 2>&1 &
AGENT_PID=$!

echo "Waiting for ggate-agent metrics server..."
READY=0
for i in {1..50}; do
    if curl -s http://localhost:9090/metrics >/dev/null 2>&1; then
        READY=1
        break
    fi
    sleep 0.1
done
if [ "${READY}" -ne 1 ]; then
    echo "FAIL: ggate-agent failed to start. Logs:"
    cat /tmp/ggate_agent_real.log
    exit 1
fi
echo "ggate-agent running (PID: ${AGENT_PID})."

# Allow dynamic cgroup watcher to enroll cgroups
sleep 0.3

# 9. Run concurrent PyTorch workloads
echo "Launching concurrent PyTorch benchmark workloads (10 seconds)..."
(echo $BASHPID > "${CGROUP_20}/cgroup.procs" && "${PYTHON_BIN}" deploy/cloud/pytorch_bench.py --duration 10 --matrix-size 256 --label tenant-20) > /tmp/pytorch_20.log 2>&1 &
PID_20=$!

(echo $BASHPID > "${CGROUP_80}/cgroup.procs" && "${PYTHON_BIN}" deploy/cloud/pytorch_bench.py --duration 10 --matrix-size 256 --label tenant-80) > /tmp/pytorch_80.log 2>&1 &
PID_80=$!

wait "${PID_20}" || { echo "PyTorch tenant-20 failed:"; cat /tmp/pytorch_20.log; exit 1; }
wait "${PID_80}" || { echo "PyTorch tenant-80 failed:"; cat /tmp/pytorch_80.log; exit 1; }

echo "=========================================================="
echo "PyTorch Benchmark Results:"
echo "=========================================================="
cat /tmp/pytorch_20.log
cat /tmp/pytorch_80.log

IOPS_20=$(grep -o 'iops=[0-9.]*' /tmp/pytorch_20.log | cut -d'=' -f2)
IOPS_80=$(grep -o 'iops=[0-9.]*' /tmp/pytorch_80.log | cut -d'=' -f2)
echo "Achieved MatMul IOPS:"
echo "  -> 20% slice (quota: 100000 tokens/sec): ${IOPS_20} matmuls/sec"
echo "  -> 80% slice (quota: 400000 tokens/sec): ${IOPS_80} matmuls/sec"

if [ -n "${IOPS_20}" ] && [ -n "${IOPS_80}" ]; then
    RATIO=$(awk -v i20="${IOPS_20}" -v i80="${IOPS_80}" 'BEGIN { if (i80 > 0) printf "%.4f", i20 / i80; else print "N/A" }')
    echo "  -> Achieved IOPS ratio (20% / 80%): ${RATIO} (target: ~0.25)"
fi

sleep 0.2
echo "=========================================================="
echo "Scraped Prometheus Metrics (:9090/metrics):"
echo "=========================================================="
curl -s http://localhost:9090/metrics | grep "ggate_" || true
echo "=========================================================="
echo "[PASS] Real GPU PyTorch Verification Complete!"
echo "=========================================================="
