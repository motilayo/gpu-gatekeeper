#!/usr/bin/env bash
set -euo pipefail

# G-Gate Third-Party Validation & Reproducibility Suite
# Executes comprehensive real-hardware benchmark suite on NVIDIA GPUs:
#   1. Attribution Overhead (Native vs. G-Gate Attribution-Only)
#   2. Multi-Tenant QoS Precision Matrix (50/50, 20/80, 10/30/60 splits)
#   3. Real PyTorch CUDA Graph Replay QoS (cuGraphLaunch)
#   4. Live VRAM Allocation, Quota Violation & cuMemFree_v2 Deallocation Lifecycle

if [ "$EUID" -ne 0 ]; then
    echo "Re-running as root via sudo..."
    exec sudo bash "$0" "$@"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${ROOT_DIR}"

OUT_DIR="${ROOT_DIR}/benchmarks/results/latest"
mkdir -p "${OUT_DIR}"

AGENT_PID=""

cleanup() {
    echo ""
    echo "=== Running Benchmark Suite Cleanup ==="
    if [ -n "${AGENT_PID:-}" ] && kill -0 "${AGENT_PID}" 2>/dev/null; then
        echo "Stopping ggate-agent (PID: ${AGENT_PID})..."
        kill -TERM "${AGENT_PID}" 2>/dev/null || true
        wait "${AGENT_PID}" 2>/dev/null || true
    fi
    pkill -9 -f "pytorch_bench.py" 2>/dev/null || true
    pkill -9 -f "build/ggate-agent" 2>/dev/null || true
    for cg in /sys/fs/cgroup/ggate-* /sys/fs/cgroup/kubepods-pod99999999_8888_7777_6666_555555555555-vramtest; do
        if [ -d "$cg" ]; then
            rmdir "$cg" 2>/dev/null || true
        fi
    done
    rm -f /tmp/bench_*.json /tmp/ggate_*.log /tmp/pytorch_*.log /tmp/bench_pods.json
}
trap cleanup EXIT INT TERM

echo "=========================================================="
echo "G-Gate Publication-Grade Validation & Reproducibility Suite"
echo "Results Directory: ${OUT_DIR}"
echo "=========================================================="

# 1. Environment & Hardware Fingerprint
echo "==> Recording Environment & Hardware Fingerprint..."
ENV_FILE="${OUT_DIR}/environment.txt"
{
    echo "=== G-Gate Benchmark Environment Fingerprint ==="
    echo "Timestamp (UTC): $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
    echo "Git Commit: $(git rev-parse HEAD 2>/dev/null || echo 'unknown')"
    echo "Kernel: $(uname -a)"
    if command -v lscpu &>/dev/null; then
        echo "CPU Model: $(lscpu | grep "Model name:" | sed 's/Model name:[ \t]*//' || true)"
    else
        echo "CPU Model: $(grep -m1 "model name" /proc/cpuinfo | cut -d: -f2 | sed 's/^[ \t]*//' || true)"
    fi
    echo "Kernel BTF: $(ls -lh /sys/kernel/btf/vmlinux 2>/dev/null || echo 'missing')"
    if [ -f "/sys/kernel/btf/vmlinux" ]; then
        echo "Kernel BTF SHA256: $(sha256sum /sys/kernel/btf/vmlinux | awk '{print $1}')"
    fi
    if command -v nvidia-smi &>/dev/null; then
        echo "NVIDIA GPU: $(nvidia-smi -L | tr '\n' ' ')"
        echo ""
        echo "GPU Details:"
        nvidia-smi --query-gpu=name,uuid,driver_version,memory.total,compute_cap --format=csv
    fi
} > "${ENV_FILE}"

# Discover Python interpreter with PyTorch
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

PYTORCH_VER=$("${PYTHON_BIN}" -c "import torch; print(f'{torch.__version__} (CUDA {torch.version.cuda})')")
echo "Python Interpreter: ${PYTHON_BIN}" >> "${ENV_FILE}"
echo "PyTorch Version: ${PYTORCH_VER}" >> "${ENV_FILE}"

# Discover libcuda.so.1
LIBCUDA_PATH=""
if command -v ldconfig &>/dev/null; then
    LIBCUDA_PATH=$(ldconfig -p 2>/dev/null | awk '/libcuda\.so\.1 / {print $NF; exit}')
fi
if [ -z "${LIBCUDA_PATH}" ] || [ ! -f "${LIBCUDA_PATH}" ]; then
    for p in /run/nvidia/driver/usr/lib/x86_64-linux-gnu/libcuda.so.1 /usr/lib/x86_64-linux-gnu/libcuda.so.1 /lib/x86_64-linux-gnu/libcuda.so.1 /usr/local/nvidia/lib64/libcuda.so.1 /usr/lib64/libcuda.so.1; do
        if [ -f "$p" ]; then LIBCUDA_PATH="$p"; break; fi
    done
fi
echo "CUDA Library: ${LIBCUDA_PATH}" >> "${ENV_FILE}"
if [ -n "${LIBCUDA_PATH}" ] && [ -f "${LIBCUDA_PATH}" ]; then
    echo "CUDA Library SHA256: $(sha256sum "${LIBCUDA_PATH}" | awk '{print $1}')" >> "${ENV_FILE}"
fi

# Ensure Go 1.24+ is installed
if ! command -v go &>/dev/null || ! go version | grep -qE 'go1\.(2[4-9]|[3-9][0-9])'; then
    echo "Installing Go from go.dev..."
    ARCH=$(uname -m)
    if [ "${ARCH}" = "x86_64" ]; then GO_ARCH="amd64"; else GO_ARCH="arm64"; fi
    GO_VER=$(curl -sL "https://go.dev/VERSION?m=text" | head -n1)
    curl -fsSL "https://go.dev/dl/${GO_VER}.linux-${GO_ARCH}.tar.gz" -o /tmp/go.tar.gz
    rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tar.gz
    ln -sf /usr/local/go/bin/go /usr/bin/go
    ln -sf /usr/local/go/bin/gofmt /usr/bin/gofmt
    rm -f /tmp/go.tar.gz
fi
echo "Go Version: $(go version)" >> "${ENV_FILE}"
cat "${ENV_FILE}"

# 2. Build ggate-agent
echo ""
echo "==> Building ggate-agent..."
mkdir -p build
go build -o build/ggate-agent ./cmd/ggate-agent
echo "Built build/ggate-agent successfully."

# Helper functions for metrics and JSON extraction
json_val() {
    local file="$1"
    local field="$2"
    "${PYTHON_BIN}" -c "import json; d = json.load(open('${file}')); print(d.get('${field}', 0))" 2>/dev/null || echo "0"
}

get_cgroup_metric() {
    local metric="$1"
    local cgroup_name="$2"
    curl -s http://localhost:9090/metrics | grep "^${metric}{" | grep "${cgroup_name}" | awk '{print $2}' | tail -n1 || echo "0"
}

wait_for_metrics() {
    local ready=0
    for i in {1..50}; do
        if curl -s http://localhost:9090/metrics >/dev/null 2>&1; then
            ready=1
            break
        fi
        sleep 0.1
    done
    if [ "$ready" -ne 1 ]; then
        echo "ERROR: ggate-agent metrics endpoint failed to start."
        exit 1
    fi
}

# Initialize CSV results files
echo "mode,trial,iters,elapsed_sec,iops,p50_ms,p95_ms,p99_ms,intercepted_launches" > "${OUT_DIR}/overhead_trials.csv"
echo "scenario,trial,tenant,target_share_pct,quota_tokens_per_sec,iters,elapsed_sec,iops,p50_ms,p95_ms,p99_ms,launches_total,throttled_total,throttled_seconds_total,vram_allocated_bytes" > "${OUT_DIR}/qos_matrix_trials.csv"
echo "tenant,quota_tokens_per_sec,iters,elapsed_sec,iops,p50_ms,p95_ms,p99_ms,launches_total,throttled_total,throttled_seconds_total" > "${OUT_DIR}/cuda_graph_trials.csv"
echo "phase,vram_allocated_bytes,vram_quota_bytes,vram_quota_exceeded_total" > "${OUT_DIR}/vram_lifecycle.csv"

# ==========================================================
# Experiment 1: eBPF Attribution Overhead
# ==========================================================
echo ""
echo "=========================================================="
echo "Experiment 1: eBPF Attribution Overhead (Native vs. G-Gate)"
echo "Matrix Size: 512x512, Duration: 5.0s, Trials: 3"
echo "=========================================================="

# Mode: Native (no_agent)
echo "--- Mode: Native (no_agent, 3 trials) ---"
for trial in 1 2 3; do
    echo "Running native trial ${trial}/3..."
    "${PYTHON_BIN}" deploy/cloud/pytorch_bench.py \
        --duration 5.0 \
        --matrix-size 512 \
        --vram-mb 128 \
        --label "native-t${trial}" \
        --json-out "/tmp/bench_overhead.json"
    
    ITERS=$(json_val "/tmp/bench_overhead.json" "iters")
    ELAPSED=$(json_val "/tmp/bench_overhead.json" "elapsed_sec")
    IOPS=$(json_val "/tmp/bench_overhead.json" "iops")
    P50=$(json_val "/tmp/bench_overhead.json" "latency_p50_ms")
    P95=$(json_val "/tmp/bench_overhead.json" "latency_p95_ms")
    P99=$(json_val "/tmp/bench_overhead.json" "latency_p99_ms")

    echo "no_agent,${trial},${ITERS},${ELAPSED},${IOPS},${P50},${P95},${P99},0" >> "${OUT_DIR}/overhead_trials.csv"
    sleep 0.5
done

# Mode: G-Gate Attribution-Only (refill_rate == 0)
echo "--- Mode: G-Gate Attribution-Only (3 trials) ---"
CG_ATTR="/sys/fs/cgroup/ggate-bench-attr"
mkdir -p "${CG_ATTR}"

./build/ggate-agent --libcuda auto --metrics-addr :9090 > /tmp/ggate_overhead.log 2>&1 &
AGENT_PID=$!
wait_for_metrics
sleep 0.3

for trial in 1 2 3; do
    echo "Running ggate attribution-only trial ${trial}/3..."
    L_PREV=$(get_cgroup_metric "ggate_gpu_launches_total" "ggate-bench-attr")
    
    (echo $BASHPID > "${CG_ATTR}/cgroup.procs" && "${PYTHON_BIN}" deploy/cloud/pytorch_bench.py \
        --duration 5.0 \
        --matrix-size 512 \
        --vram-mb 128 \
        --label "ggate-attr-t${trial}" \
        --json-out "/tmp/bench_overhead.json")
    
    sleep 0.2
    L_POST=$(get_cgroup_metric "ggate_gpu_launches_total" "ggate-bench-attr")
    L_DELTA=$(awk -v pre="${L_PREV:-0}" -v post="${L_POST:-0}" 'BEGIN { print (post - pre) }')

    ITERS=$(json_val "/tmp/bench_overhead.json" "iters")
    ELAPSED=$(json_val "/tmp/bench_overhead.json" "elapsed_sec")
    IOPS=$(json_val "/tmp/bench_overhead.json" "iops")
    P50=$(json_val "/tmp/bench_overhead.json" "latency_p50_ms")
    P95=$(json_val "/tmp/bench_overhead.json" "latency_p95_ms")
    P99=$(json_val "/tmp/bench_overhead.json" "latency_p99_ms")

    echo "ggate_attribution_only,${trial},${ITERS},${ELAPSED},${IOPS},${P50},${P95},${P99},${L_DELTA}" >> "${OUT_DIR}/overhead_trials.csv"
    sleep 0.5
done

kill -TERM "${AGENT_PID}"
wait "${AGENT_PID}" 2>/dev/null || true
AGENT_PID=""
rmdir "${CG_ATTR}" 2>/dev/null || true

# ==========================================================
# Experiment 2: Multi-Tenant QoS Precision Matrix
# ==========================================================
echo ""
echo "=========================================================="
echo "Experiment 2: Multi-Tenant QoS Precision Matrix"
echo "Matrix Size: 256x256, Duration: 6.0s per trial, 3 trials each"
echo "=========================================================="

# Scenario A: 50/50 Equal Split (200k tokens/sec each)
echo "--- Scenario A: 50/50 Equal Split (200k / 200k tokens/sec) ---"
CG_50A="/sys/fs/cgroup/ggate-50a"
CG_50B="/sys/fs/cgroup/ggate-50b"
mkdir -p "${CG_50A}" "${CG_50B}"

./build/ggate-agent \
    --libcuda auto \
    --metrics-addr :9090 \
    --quota "${CG_50A}:200000:20000" \
    --quota "${CG_50B}:200000:20000" \
    > /tmp/ggate_50_50.log 2>&1 &
AGENT_PID=$!
wait_for_metrics
sleep 0.3

for trial in 1 2 3; do
    echo "Running 50/50 trial ${trial}/3..."
    (echo $BASHPID > "${CG_50A}/cgroup.procs" && "${PYTHON_BIN}" deploy/cloud/pytorch_bench.py --duration 6.0 --matrix-size 256 --label "tenant-50a" --json-out "/tmp/bench_50a.json") > /tmp/pytorch_50a.log 2>&1 &
    PID_A=$!
    (echo $BASHPID > "${CG_50B}/cgroup.procs" && "${PYTHON_BIN}" deploy/cloud/pytorch_bench.py --duration 6.0 --matrix-size 256 --label "tenant-50b" --json-out "/tmp/bench_50b.json") > /tmp/pytorch_50b.log 2>&1 &
    PID_B=$!

    wait "${PID_A}"
    wait "${PID_B}"
    sleep 0.2

    for t in "50a:tenant-50a:50:200000:/tmp/bench_50a.json" "50b:tenant-50b:50:200000:/tmp/bench_50b.json"; do
        IFS=":" read -r cg_name t_name share quota jfile <<< "${t}"
        ITERS=$(json_val "${jfile}" "iters")
        ELAPSED=$(json_val "${jfile}" "elapsed_sec")
        IOPS=$(json_val "${jfile}" "iops")
        P50=$(json_val "${jfile}" "latency_p50_ms")
        P95=$(json_val "${jfile}" "latency_p95_ms")
        P99=$(json_val "${jfile}" "latency_p99_ms")
        LAUNCHES=$(get_cgroup_metric "ggate_gpu_launches_total" "ggate-${cg_name}")
        THROTTLED=$(get_cgroup_metric "ggate_gpu_throttled_total" "ggate-${cg_name}")
        THROTTLED_S=$(get_cgroup_metric "ggate_gpu_throttled_seconds_total" "ggate-${cg_name}")
        VRAM=$(get_cgroup_metric "ggate_vram_allocated_bytes" "ggate-${cg_name}")

        echo "50_50_equal_split,${trial},${t_name},${share},${quota},${ITERS},${ELAPSED},${IOPS},${P50},${P95},${P99},${LAUNCHES:-0},${THROTTLED:-0},${THROTTLED_S:-0},${VRAM:-0}" >> "${OUT_DIR}/qos_matrix_trials.csv"
    done
    sleep 0.5
done

curl -s http://localhost:9090/metrics > "${OUT_DIR}/prom_50_50.txt"
kill -TERM "${AGENT_PID}"
wait "${AGENT_PID}" 2>/dev/null || true
AGENT_PID=""
rmdir "${CG_50A}" "${CG_50B}" 2>/dev/null || true

# Scenario B: 20/80 Asymmetric Split (100k vs 400k tokens/sec)
echo "--- Scenario B: 20/80 Asymmetric Split (100k / 400k tokens/sec) ---"
CG_20="/sys/fs/cgroup/ggate-20"
CG_80="/sys/fs/cgroup/ggate-80"
mkdir -p "${CG_20}" "${CG_80}"

./build/ggate-agent \
    --libcuda auto \
    --metrics-addr :9090 \
    --quota "${CG_20}:100000:10000" \
    --quota "${CG_80}:400000:40000" \
    > /tmp/ggate_20_80.log 2>&1 &
AGENT_PID=$!
wait_for_metrics
sleep 0.3

for trial in 1 2 3; do
    echo "Running 20/80 trial ${trial}/3..."
    (echo $BASHPID > "${CG_20}/cgroup.procs" && "${PYTHON_BIN}" deploy/cloud/pytorch_bench.py --duration 6.0 --matrix-size 256 --label "tenant-20" --json-out "/tmp/bench_20.json") > /tmp/pytorch_20.log 2>&1 &
    PID_20=$!
    (echo $BASHPID > "${CG_80}/cgroup.procs" && "${PYTHON_BIN}" deploy/cloud/pytorch_bench.py --duration 6.0 --matrix-size 256 --label "tenant-80" --json-out "/tmp/bench_80.json") > /tmp/pytorch_80.log 2>&1 &
    PID_80=$!

    wait "${PID_20}"
    wait "${PID_80}"
    sleep 0.2

    for t in "20:tenant-20:20:100000:/tmp/bench_20.json" "80:tenant-80:80:400000:/tmp/bench_80.json"; do
        IFS=":" read -r cg_name t_name share quota jfile <<< "${t}"
        ITERS=$(json_val "${jfile}" "iters")
        ELAPSED=$(json_val "${jfile}" "elapsed_sec")
        IOPS=$(json_val "${jfile}" "iops")
        P50=$(json_val "${jfile}" "latency_p50_ms")
        P95=$(json_val "${jfile}" "latency_p95_ms")
        P99=$(json_val "${jfile}" "latency_p99_ms")
        LAUNCHES=$(get_cgroup_metric "ggate_gpu_launches_total" "ggate-${cg_name}")
        THROTTLED=$(get_cgroup_metric "ggate_gpu_throttled_total" "ggate-${cg_name}")
        THROTTLED_S=$(get_cgroup_metric "ggate_gpu_throttled_seconds_total" "ggate-${cg_name}")
        VRAM=$(get_cgroup_metric "ggate_vram_allocated_bytes" "ggate-${cg_name}")

        echo "20_80_asymmetric_split,${trial},${t_name},${share},${quota},${ITERS},${ELAPSED},${IOPS},${P50},${P95},${P99},${LAUNCHES:-0},${THROTTLED:-0},${THROTTLED_S:-0},${VRAM:-0}" >> "${OUT_DIR}/qos_matrix_trials.csv"
    done
    sleep 0.5
done

curl -s http://localhost:9090/metrics > "${OUT_DIR}/prom_20_80.txt"
kill -TERM "${AGENT_PID}"
wait "${AGENT_PID}" 2>/dev/null || true
AGENT_PID=""
rmdir "${CG_20}" "${CG_80}" 2>/dev/null || true

# Scenario C: 10/30/60 Three-Tenant Split (50k / 150k / 300k tokens/sec)
echo "--- Scenario C: 10/30/60 Three-Tenant Split ---"
CG_10="/sys/fs/cgroup/ggate-10"
CG_30="/sys/fs/cgroup/ggate-30"
CG_60="/sys/fs/cgroup/ggate-60"
mkdir -p "${CG_10}" "${CG_30}" "${CG_60}"

./build/ggate-agent \
    --libcuda auto \
    --metrics-addr :9090 \
    --quota "${CG_10}:50000:5000" \
    --quota "${CG_30}:150000:15000" \
    --quota "${CG_60}:300000:30000" \
    > /tmp/ggate_10_30_60.log 2>&1 &
AGENT_PID=$!
wait_for_metrics
sleep 0.3

for trial in 1 2 3; do
    echo "Running 10/30/60 trial ${trial}/3..."
    (echo $BASHPID > "${CG_10}/cgroup.procs" && "${PYTHON_BIN}" deploy/cloud/pytorch_bench.py --duration 6.0 --matrix-size 256 --label "tenant-10" --json-out "/tmp/bench_10.json") > /tmp/pytorch_10.log 2>&1 &
    PID_10=$!
    (echo $BASHPID > "${CG_30}/cgroup.procs" && "${PYTHON_BIN}" deploy/cloud/pytorch_bench.py --duration 6.0 --matrix-size 256 --label "tenant-30" --json-out "/tmp/bench_30.json") > /tmp/pytorch_30.log 2>&1 &
    PID_30=$!
    (echo $BASHPID > "${CG_60}/cgroup.procs" && "${PYTHON_BIN}" deploy/cloud/pytorch_bench.py --duration 6.0 --matrix-size 256 --label "tenant-60" --json-out "/tmp/bench_60.json") > /tmp/pytorch_60.log 2>&1 &
    PID_60=$!

    wait "${PID_10}"
    wait "${PID_30}"
    wait "${PID_60}"
    sleep 0.2

    for t in "10:tenant-10:10:50000:/tmp/bench_10.json" "30:tenant-30:30:150000:/tmp/bench_30.json" "60:tenant-60:60:300000:/tmp/bench_60.json"; do
        IFS=":" read -r cg_name t_name share quota jfile <<< "${t}"
        ITERS=$(json_val "${jfile}" "iters")
        ELAPSED=$(json_val "${jfile}" "elapsed_sec")
        IOPS=$(json_val "${jfile}" "iops")
        P50=$(json_val "${jfile}" "latency_p50_ms")
        P95=$(json_val "${jfile}" "latency_p95_ms")
        P99=$(json_val "${jfile}" "latency_p99_ms")
        LAUNCHES=$(get_cgroup_metric "ggate_gpu_launches_total" "ggate-${cg_name}")
        THROTTLED=$(get_cgroup_metric "ggate_gpu_throttled_total" "ggate-${cg_name}")
        THROTTLED_S=$(get_cgroup_metric "ggate_gpu_throttled_seconds_total" "ggate-${cg_name}")
        VRAM=$(get_cgroup_metric "ggate_vram_allocated_bytes" "ggate-${cg_name}")

        echo "10_30_60_three_tenant_split,${trial},${t_name},${share},${quota},${ITERS},${ELAPSED},${IOPS},${P50},${P95},${P99},${LAUNCHES:-0},${THROTTLED:-0},${THROTTLED_S:-0},${VRAM:-0}" >> "${OUT_DIR}/qos_matrix_trials.csv"
    done
    sleep 0.5
done

curl -s http://localhost:9090/metrics > "${OUT_DIR}/prom_10_30_60.txt"
kill -TERM "${AGENT_PID}"
wait "${AGENT_PID}" 2>/dev/null || true
AGENT_PID=""
rmdir "${CG_10}" "${CG_30}" "${CG_60}" 2>/dev/null || true

# ==========================================================
# Experiment 3: Real PyTorch CUDA Graph Replay QoS
# ==========================================================
echo ""
echo "=========================================================="
echo "Experiment 3: PyTorch torch.cuda.CUDAGraph Replay QoS"
echo "cuGraphLaunch rate limiting: 6,400 (20%) vs 25,600 (80%)"
echo "=========================================================="
CG_G20="/sys/fs/cgroup/ggate-graph-20"
CG_G80="/sys/fs/cgroup/ggate-graph-80"
mkdir -p "${CG_G20}" "${CG_G80}"

./build/ggate-agent \
    --libcuda auto \
    --metrics-addr :9090 \
    --quota "${CG_G20}:6400:64" \
    --quota "${CG_G80}:25600:256" \
    > /tmp/ggate_graph.log 2>&1 &
AGENT_PID=$!
wait_for_metrics
sleep 0.3

echo "Running concurrent CUDA Graph workloads (6 seconds)..."
(echo $BASHPID > "${CG_G20}/cgroup.procs" && "${PYTHON_BIN}" deploy/cloud/pytorch_bench.py --duration 6.0 --matrix-size 256 --cuda-graph --label "tenant-graph-20" --json-out "/tmp/bench_graph_20.json") > /tmp/pytorch_g20.log 2>&1 &
PID_G20=$!
(echo $BASHPID > "${CG_G80}/cgroup.procs" && "${PYTHON_BIN}" deploy/cloud/pytorch_bench.py --duration 6.0 --matrix-size 256 --cuda-graph --label "tenant-graph-80" --json-out "/tmp/bench_graph_80.json") > /tmp/pytorch_g80.log 2>&1 &
PID_G80=$!

wait "${PID_G20}"
wait "${PID_G80}"
sleep 0.2

for t in "graph-20:tenant-graph-20:6400:/tmp/bench_graph_20.json" "graph-80:tenant-graph-80:25600:/tmp/bench_graph_80.json"; do
    IFS=":" read -r cg_name t_name quota jfile <<< "${t}"
    ITERS=$(json_val "${jfile}" "iters")
    ELAPSED=$(json_val "${jfile}" "elapsed_sec")
    IOPS=$(json_val "${jfile}" "iops")
    P50=$(json_val "${jfile}" "latency_p50_ms")
    P95=$(json_val "${jfile}" "latency_p95_ms")
    P99=$(json_val "${jfile}" "latency_p99_ms")
    LAUNCHES=$(get_cgroup_metric "ggate_gpu_launches_total" "ggate-${cg_name}")
    THROTTLED=$(get_cgroup_metric "ggate_gpu_throttled_total" "ggate-${cg_name}")
    THROTTLED_S=$(get_cgroup_metric "ggate_gpu_throttled_seconds_total" "ggate-${cg_name}")

    echo "${t_name},${quota},${ITERS},${ELAPSED},${IOPS},${P50},${P95},${P99},${LAUNCHES:-0},${THROTTLED:-0},${THROTTLED_S:-0}" >> "${OUT_DIR}/cuda_graph_trials.csv"
done

curl -s http://localhost:9090/metrics > "${OUT_DIR}/prom_cuda_graph.txt"
kill -TERM "${AGENT_PID}"
wait "${AGENT_PID}" 2>/dev/null || true
AGENT_PID=""
rmdir "${CG_G20}" "${CG_G80}" 2>/dev/null || true

# ==========================================================
# Experiment 4: Live VRAM Lifecycle & Quota Violation
# ==========================================================
echo ""
echo "=========================================================="
echo "Experiment 4: Live VRAM Lifecycle & Quota Violation Audit"
echo "Allocates 256 MiB with 128 MiB Quota annotation"
echo "=========================================================="

cat << 'EOF' > /tmp/bench_pods.json
{
  "items": [
    {
      "metadata": {
        "name": "vllm-vram-audit",
        "namespace": "ai-prod",
        "uid": "99999999-8888-7777-6666-555555555555",
        "annotations": {
          "g-gate.io/max-vram-mb": "128"
        }
      },
      "status": {
        "containerStatuses": [
          {
            "name": "vllm",
            "containerID": "containerd://vramtest"
          }
        ]
      }
    }
  ]
}
EOF

CG_VRAM="/sys/fs/cgroup/kubepods-pod99999999_8888_7777_6666_555555555555-vramtest"
mkdir -p "${CG_VRAM}"

./build/ggate-agent \
    --libcuda auto \
    --metrics-addr :9090 \
    --kubelet-pods-file /tmp/bench_pods.json \
    > /tmp/ggate_vram.log 2>&1 &
AGENT_PID=$!
wait_for_metrics
sleep 0.3

echo "Starting PyTorch workload with 256 MiB allocation inside Pod cgroup..."
(echo $BASHPID > "${CG_VRAM}/cgroup.procs" && "${PYTHON_BIN}" deploy/cloud/pytorch_bench.py --duration 4.0 --vram-mb 256 --free-on-exit --label "vram-audit") > /tmp/pytorch_vram.log 2>&1 &
PID_VRAM=$!

# Poll until PyTorch completes CUDA context init and allocates the 256 MiB tensor
VRAM_ALLOC_DURING="0"
for i in {1..40}; do
    VRAM_ALLOC_DURING=$(get_cgroup_metric "ggate_vram_allocated_bytes" "vllm-vram-audit")
    if [ -n "${VRAM_ALLOC_DURING}" ] && [ "${VRAM_ALLOC_DURING}" != "0" ]; then
        break
    fi
    sleep 0.2
done
sleep 0.3
VRAM_ALLOC_DURING=$(get_cgroup_metric "ggate_vram_allocated_bytes" "vllm-vram-audit")
VRAM_QUOTA_DURING=$(get_cgroup_metric "ggate_vram_quota_bytes" "vllm-vram-audit")
VRAM_EXCEED_DURING=$(get_cgroup_metric "ggate_vram_quota_exceeded_total" "vllm-vram-audit")

echo "during_allocation,${VRAM_ALLOC_DURING:-0},${VRAM_QUOTA_DURING:-0},${VRAM_EXCEED_DURING:-0}" >> "${OUT_DIR}/vram_lifecycle.csv"

# Wait for workload to finish and call cuMemFree_v2
wait "${PID_VRAM}"
sleep 0.3

VRAM_ALLOC_AFTER=$(get_cgroup_metric "ggate_vram_allocated_bytes" "vllm-vram-audit")
VRAM_QUOTA_AFTER=$(get_cgroup_metric "ggate_vram_quota_bytes" "vllm-vram-audit")
VRAM_EXCEED_AFTER=$(get_cgroup_metric "ggate_vram_quota_exceeded_total" "vllm-vram-audit")

echo "after_free,${VRAM_ALLOC_AFTER:-0},${VRAM_QUOTA_AFTER:-0},${VRAM_EXCEED_AFTER:-0}" >> "${OUT_DIR}/vram_lifecycle.csv"

curl -s http://localhost:9090/metrics > "${OUT_DIR}/prom_vram_lifecycle.txt"
kill -TERM "${AGENT_PID}"
wait "${AGENT_PID}" 2>/dev/null || true
AGENT_PID=""
rmdir "${CG_VRAM}" 2>/dev/null || true
rm -f /tmp/bench_pods.json

# ==========================================================
# Print Final Summary Report
# ==========================================================
echo ""
echo "=========================================================="
echo " VALIDATION SUITE SUMMARY REPORT"
echo "=========================================================="
echo ""
echo "--- 1. Overhead Trials (benchmarks/results/latest/overhead_trials.csv) ---"
cat "${OUT_DIR}/overhead_trials.csv" | column -s, -t
echo ""
echo "--- 2. QoS Matrix Trials (benchmarks/results/latest/qos_matrix_trials.csv) ---"
cat "${OUT_DIR}/qos_matrix_trials.csv" | column -s, -t
echo ""
echo "--- 3. CUDA Graph Trials (benchmarks/results/latest/cuda_graph_trials.csv) ---"
cat "${OUT_DIR}/cuda_graph_trials.csv" | column -s, -t
echo ""
echo "--- 4. VRAM Lifecycle (benchmarks/results/latest/vram_lifecycle.csv) ---"
cat "${OUT_DIR}/vram_lifecycle.csv" | column -s, -t
echo ""
echo "=========================================================="
echo " Validation suite completed successfully. Raw data saved in ${OUT_DIR}."
echo "=========================================================="
