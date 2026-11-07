#!/usr/bin/env bash
set -euo pipefail

# G-Gate Real LLM-Serving QoS Benchmark (Experiment 5)
# Measures TTFT, Inter-Token Latency (ITL), and Generated Tokens/sec across:
#   A. Isolated Baseline (1 tenant, unconstrained)
#   B. Contended No G-Gate (2 tenants concurrent, unconstrained)
#   C. Contended G-Gate 20/80 (2 tenants in cgroups ggate-llm-20 / ggate-llm-80)

if [ "$EUID" -ne 0 ]; then
    echo "Re-running as root via sudo..."
    exec sudo bash "$0" "$@"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${ROOT_DIR}"

OUT_DIR="${ROOT_DIR}/benchmarks/results/llm"
mkdir -p "${OUT_DIR}"

export HF_HOME="${HF_HOME:-/opt/dlami/nvme/huggingface}"
mkdir -p "${HF_HOME}"

DURATION="${DURATION:-60}"
TRIALS="${TRIALS:-3}"
MODEL_NAME="${MODEL_NAME:-Qwen/Qwen2.5-0.5B-Instruct}"
FALLBACK_MODEL="${FALLBACK_MODEL:-TinyLlama/TinyLlama-1.1B-Chat-v1.0}"

CGROUP_A="/sys/fs/cgroup/ggate-llm-isolated"
CGROUP_B1="/sys/fs/cgroup/ggate-llm-contended-1"
CGROUP_B2="/sys/fs/cgroup/ggate-llm-contended-2"
CGROUP_20="/sys/fs/cgroup/ggate-llm-20"
CGROUP_80="/sys/fs/cgroup/ggate-llm-80"

AGENT_PID=""

cleanup() {
    echo ""
    echo "=== Cleaning up benchmark processes and cgroups ==="
    if [ -n "${AGENT_PID:-}" ] && kill -0 "${AGENT_PID}" 2>/dev/null; then
        echo "Stopping ggate-agent (PID: ${AGENT_PID})..."
        kill -TERM "${AGENT_PID}" 2>/dev/null || true
        wait "${AGENT_PID}" 2>/dev/null || true
    fi
    pkill -9 -f "llm_bench.py" 2>/dev/null || true
    pkill -9 -f "build/ggate-agent" 2>/dev/null || true
    for cg in "${CGROUP_A}" "${CGROUP_B1}" "${CGROUP_B2}" "${CGROUP_20}" "${CGROUP_80}"; do
        if [ -d "$cg" ]; then
            rmdir "$cg" 2>/dev/null || true
        fi
    done
    rm -f /tmp/llm_bench_*.log /tmp/ggate_agent_*.log
}
trap cleanup EXIT INT TERM

echo "=========================================================="
echo "G-Gate Real LLM-Serving QoS Benchmark (Experiment 5)"
echo "Output Directory: ${OUT_DIR}"
echo "Model: ${MODEL_NAME} (Fallback: ${FALLBACK_MODEL})"
echo "Scenario Duration: ${DURATION}s, Trials: ${TRIALS}"
echo "=========================================================="

# 1. Discover Python & ensure dependencies
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
echo "Using Python: ${PYTHON_BIN}"

# Check/install transformers and accelerate
if ! "${PYTHON_BIN}" -c "import transformers, accelerate" &>/dev/null; then
    echo "Installing transformers and accelerate..."
    "${PYTHON_BIN}" -m pip install --quiet transformers accelerate
fi

# 2. Discover libcuda.so.1
LIBCUDA_PATH=""
if command -v ldconfig &>/dev/null; then
    LIBCUDA_PATH=$(ldconfig -p 2>/dev/null | awk '/libcuda\.so\.1 / {print $NF; exit}')
fi
if [ -z "${LIBCUDA_PATH}" ] || [ ! -f "${LIBCUDA_PATH}" ]; then
    for p in /usr/lib/x86_64-linux-gnu/libcuda.so.1 /usr/local/nvidia/lib64/libcuda.so.1 /usr/lib64/libcuda.so.1 /run/nvidia/driver/usr/lib/x86_64-linux-gnu/libcuda.so.1; do
        if [ -f "$p" ]; then LIBCUDA_PATH="$p"; break; fi
    done
fi
echo "Located CUDA Driver Library: ${LIBCUDA_PATH}"

# 3. Environment fingerprint
ENV_FILE="${OUT_DIR}/environment.txt"
{
    echo "=== G-Gate Real LLM Benchmark Environment Fingerprint ==="
    echo "Timestamp (UTC): $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
    echo "Git Commit: $(git rev-parse HEAD 2>/dev/null || echo 'unknown')"
    echo "Kernel: $(uname -a)"
    if command -v nvidia-smi &>/dev/null; then
        echo "NVIDIA GPU: $(nvidia-smi -L | tr '\n' ' ')"
        echo ""
        echo "GPU Details:"
        nvidia-smi --query-gpu=name,uuid,driver_version,memory.total,compute_cap --format=csv
    fi
    echo ""
    echo "Python Interpreter: ${PYTHON_BIN}"
    echo "PyTorch Version: $("${PYTHON_BIN}" -c 'import torch; print(f"{torch.__version__} (CUDA {torch.version.cuda})")')"
    echo "Transformers Version: $("${PYTHON_BIN}" -c 'import transformers; print(transformers.__version__)')"
    echo "Accelerate Version: $("${PYTHON_BIN}" -c 'import accelerate; print(accelerate.__version__)')"
    echo "CUDA Driver Library: ${LIBCUDA_PATH}"
    if [ -f "${LIBCUDA_PATH}" ]; then
        echo "libcuda.so.1 SHA256: $(sha256sum "${LIBCUDA_PATH}" | awk '{print $1}')"
    fi
    echo "Model Configured: ${MODEL_NAME} (fallback: ${FALLBACK_MODEL})"
} > "${ENV_FILE}"
cat "${ENV_FILE}"

# 4. Check ggate-agent binary
if [ ! -f "build/ggate-agent" ]; then
    echo "Building ggate-agent binary..."
    if ! command -v go &>/dev/null; then
        echo "Installing Go..."
        ARCH=$(uname -m)
        if [ "${ARCH}" = "x86_64" ]; then GO_ARCH="amd64"; else GO_ARCH="arm64"; fi
        curl -fsSL "https://go.dev/dl/go1.24.1.linux-${GO_ARCH}.tar.gz" -o /tmp/go.tar.gz
        rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tar.gz
        ln -sf /usr/local/go/bin/go /usr/bin/go
        rm -f /tmp/go.tar.gz
    fi
    mkdir -p build
    go build -o build/ggate-agent ./cmd/ggate-agent
fi
echo "Verified build/ggate-agent binary: $(ls -lh build/ggate-agent)"

# Helper function to launch ggate-agent
start_agent() {
    local quota_args=("$@")
    echo "Starting ggate-agent with quotas: ${quota_args[*]:-none}..."
    ./build/ggate-agent \
        --libcuda "${LIBCUDA_PATH}" \
        --metrics-addr :9090 \
        "${quota_args[@]}" \
        > /tmp/ggate_agent.log 2>&1 &
    AGENT_PID=$!

    local ready=0
    for i in {1..50}; do
        if curl -s http://localhost:9090/metrics >/dev/null 2>&1; then
            ready=1
            break
        fi
        sleep 0.1
    done
    if [ "${ready}" -ne 1 ]; then
        echo "FAIL: ggate-agent failed to start. Logs:"
        cat /tmp/ggate_agent.log
        exit 1
    fi
}

stop_agent() {
    if [ -n "${AGENT_PID:-}" ] && kill -0 "${AGENT_PID}" 2>/dev/null; then
        kill -TERM "${AGENT_PID}" 2>/dev/null || true
        wait "${AGENT_PID}" 2>/dev/null || true
        AGENT_PID=""
    fi
}

# ==========================================================
# Scenario A: Isolated Baseline (Single Tenant)
# ==========================================================
echo ""
echo "=========================================================="
echo "Scenario A: Isolated Baseline (1 tenant, no QoS throttling)"
echo "=========================================================="
mkdir -p "${CGROUP_A}"
CGROUP_ID_A=$(stat -c '%i' "${CGROUP_A}")

start_agent --quota "${CGROUP_A}:0:0"

for t in $(seq 1 "${TRIALS}"); do
    echo "--- Scenario A: Trial ${t}/${TRIALS} (${DURATION}s) ---"
    (echo $BASHPID > "${CGROUP_A}/cgroup.procs" && \
     "${PYTHON_BIN}" benchmarks/llm_bench.py \
        --model "${MODEL_NAME}" \
        --fallback-model "${FALLBACK_MODEL}" \
        --duration "${DURATION}" \
        --label "isolated-t${t}" \
        --json-out "${OUT_DIR}/scenario_a_isolated_trial${t}.json")
    curl -s http://localhost:9090/metrics > "${OUT_DIR}/scenario_a_prom_trial${t}.txt"
    curl -s http://localhost:9090/api/v1/tenants > "${OUT_DIR}/scenario_a_tenants_trial${t}.json" || true
done

stop_agent
rmdir "${CGROUP_A}" 2>/dev/null || true

# Extract baseline throughput for calibration
BASE_TOK_S=$("${PYTHON_BIN}" -c "import json, glob; fs=glob.glob('${OUT_DIR}/scenario_a_isolated_trial*.json'); ts=[json.load(open(f))['tokens_per_sec'] for f in fs]; print(sum(ts)/len(ts)) if ts else print('100.0')")
echo "Scenario A Baseline Throughput: ${BASE_TOK_S} tokens/sec"

# ==========================================================
# Scenario B: Contended, No G-Gate (Noisy Neighbor Baseline)
# ==========================================================
echo ""
echo "=========================================================="
echo "Scenario B: Contended, No G-Gate (2 tenants concurrent, unconstrained)"
echo "=========================================================="
mkdir -p "${CGROUP_B1}" "${CGROUP_B2}"
CGROUP_ID_B1=$(stat -c '%i' "${CGROUP_B1}")
CGROUP_ID_B2=$(stat -c '%i' "${CGROUP_B2}")

start_agent --quota "${CGROUP_B1}:0:0" --quota "${CGROUP_B2}:0:0"

for t in $(seq 1 "${TRIALS}"); do
    echo "--- Scenario B: Trial ${t}/${TRIALS} (${DURATION}s) ---"
    (echo $BASHPID > "${CGROUP_B1}/cgroup.procs" && \
     "${PYTHON_BIN}" benchmarks/llm_bench.py \
        --model "${MODEL_NAME}" \
        --fallback-model "${FALLBACK_MODEL}" \
        --duration "${DURATION}" \
        --label "contended-t1-trial${t}" \
        --json-out "${OUT_DIR}/scenario_b_tenant1_trial${t}.json") &
    PID_B1=$!

    (echo $BASHPID > "${CGROUP_B2}/cgroup.procs" && \
     "${PYTHON_BIN}" benchmarks/llm_bench.py \
        --model "${MODEL_NAME}" \
        --fallback-model "${FALLBACK_MODEL}" \
        --duration "${DURATION}" \
        --label "contended-t2-trial${t}" \
        --json-out "${OUT_DIR}/scenario_b_tenant2_trial${t}.json") &
    PID_B2=$!

    wait "${PID_B1}"
    wait "${PID_B2}"

    curl -s http://localhost:9090/metrics > "${OUT_DIR}/scenario_b_prom_trial${t}.txt"
    curl -s http://localhost:9090/api/v1/tenants > "${OUT_DIR}/scenario_b_tenants_trial${t}.json" || true
done

stop_agent
rmdir "${CGROUP_B1}" 2>/dev/null || true
rmdir "${CGROUP_B2}" 2>/dev/null || true

# ==========================================================
# Scenario C Calibration: Finding appropriate token budgets
# ==========================================================
echo ""
echo "=========================================================="
echo "Scenario C Calibration: Empirical Rate Search"
echo "=========================================================="
CALIB_LOG="${OUT_DIR}/calibration_log.txt"
echo "=== Calibration Log ===" > "${CALIB_LOG}"

mkdir -p "${CGROUP_20}" "${CGROUP_80}"
CGROUP_ID_20=$(stat -c '%i' "${CGROUP_20}")
CGROUP_ID_80=$(stat -c '%i' "${CGROUP_80}")

# Starting rate candidates (ratio 1:4)
RATE_20=400000
RATE_80=1600000
BURST_20=40000
BURST_80=160000

CALIB_ROUND=1
CALIB_DURATION=20

while [ ${CALIB_ROUND} -le 4 ]; do
    echo "--- Calibration Round ${CALIB_ROUND}: Testing 20%=${RATE_20}, 80%=${RATE_80} tokens/sec (${CALIB_DURATION}s probe) ---"
    start_agent --quota "${CGROUP_20}:${RATE_20}:${BURST_20}" --quota "${CGROUP_80}:${RATE_80}:${BURST_80}"

    (echo $BASHPID > "${CGROUP_20}/cgroup.procs" && \
     "${PYTHON_BIN}" benchmarks/llm_bench.py \
        --model "${MODEL_NAME}" \
        --fallback-model "${FALLBACK_MODEL}" \
        --duration "${CALIB_DURATION}" \
        --label "calib-20" \
        --json-out "/tmp/calib_20.json") &
    CPID_20=$!

    (echo $BASHPID > "${CGROUP_80}/cgroup.procs" && \
     "${PYTHON_BIN}" benchmarks/llm_bench.py \
        --model "${MODEL_NAME}" \
        --fallback-model "${FALLBACK_MODEL}" \
        --duration "${CALIB_DURATION}" \
        --label "calib-80" \
        --json-out "/tmp/calib_80.json") &
    CPID_80=$!

    wait "${CPID_20}"
    wait "${CPID_80}"

    METRICS=$(curl -s http://localhost:9090/metrics)
    THROT_20=$(echo "${METRICS}" | grep "^ggate_gpu_throttled_total{cgroup_id=\"${CGROUP_ID_20}\".*}" | awk '{print $2}' || echo "0")
    THROT_80=$(echo "${METRICS}" | grep "^ggate_gpu_throttled_total{cgroup_id=\"${CGROUP_ID_80}\".*}" | awk '{print $2}' || echo "0")
    TOK_80=$("${PYTHON_BIN}" -c "import json; print(json.load(open('/tmp/calib_80.json'))['tokens_per_sec'])" 2>/dev/null || echo "0")

    THROT_20="${THROT_20:-0}"
    THROT_80="${THROT_80:-0}"

    echo "Round ${CALIB_ROUND}: rate_20=${RATE_20}, rate_80=${RATE_80} -> throttled_20=${THROT_20}, throttled_80=${THROT_80}, tok_s_80=${TOK_80}" | tee -a "${CALIB_LOG}"

    stop_agent

    # Condition: 20 tenant MUST be clearly throttled (>0 throttles)
    if [ "${THROT_20}" -eq 0 ]; then
        echo "Tenant 20 was NOT throttled at rate=${RATE_20}. Dividing budgets by 4 and retrying..." | tee -a "${CALIB_LOG}"
        RATE_20=$((RATE_20 / 4))
        RATE_80=$((RATE_80 / 4))
        BURST_20=$((BURST_20 / 4))
        BURST_80=$((BURST_80 / 4))
        CALIB_ROUND=$((CALIB_ROUND + 1))
        continue
    fi

    # Condition: if 80 tenant achieves < 50% of isolated throughput, multiply by 2 (if rate allows)
    HALF_BASE=$(awk "BEGIN {print ${BASE_TOK_S} * 0.5}")
    if awk "BEGIN {exit !(${TOK_80} < ${HALF_BASE})}"; then
        echo "Tenant 80 throughput (${TOK_80} tok/s) is < 50% of baseline (${HALF_BASE} tok/s). Multiplying budgets by 2..." | tee -a "${CALIB_LOG}"
        RATE_20=$((RATE_20 * 2))
        RATE_80=$((RATE_80 * 2))
        BURST_20=$((BURST_20 * 2))
        BURST_80=$((BURST_80 * 2))
        CALIB_ROUND=$((CALIB_ROUND + 1))
        continue
    fi

    echo "Calibration success! Selected rates: 20%=${RATE_20}, 80%=${RATE_80}" | tee -a "${CALIB_LOG}"
    break
done

echo "Final Calibrated Rates: RATE_20=${RATE_20}, RATE_80=${RATE_80}" >> "${CALIB_LOG}"

# ==========================================================
# Scenario C: Contended, G-Gate 20/80 Quotas
# ==========================================================
echo ""
echo "=========================================================="
echo "Scenario C: Contended, G-Gate 20/80 (rate_20=${RATE_20}, rate_80=${RATE_80})"
echo "=========================================================="

start_agent --quota "${CGROUP_20}:${RATE_20}:${BURST_20}" --quota "${CGROUP_80}:${RATE_80}:${BURST_80}"

for t in $(seq 1 "${TRIALS}"); do
    echo "--- Scenario C: Trial ${t}/${TRIALS} (${DURATION}s) ---"
    (echo $BASHPID > "${CGROUP_20}/cgroup.procs" && \
     "${PYTHON_BIN}" benchmarks/llm_bench.py \
        --model "${MODEL_NAME}" \
        --fallback-model "${FALLBACK_MODEL}" \
        --duration "${DURATION}" \
        --label "ggate-20-trial${t}" \
        --json-out "${OUT_DIR}/scenario_c_tenant20_trial${t}.json") &
    PID_C20=$!

    (echo $BASHPID > "${CGROUP_80}/cgroup.procs" && \
     "${PYTHON_BIN}" benchmarks/llm_bench.py \
        --model "${MODEL_NAME}" \
        --fallback-model "${FALLBACK_MODEL}" \
        --duration "${DURATION}" \
        --label "ggate-80-trial${t}" \
        --json-out "${OUT_DIR}/scenario_c_tenant80_trial${t}.json") &
    PID_C80=$!

    wait "${PID_C20}"
    wait "${PID_C80}"

    curl -s http://localhost:9090/metrics > "${OUT_DIR}/scenario_c_prom_trial${t}.txt"
    curl -s http://localhost:9090/api/v1/tenants > "${OUT_DIR}/scenario_c_tenants_trial${t}.json" || true
done

stop_agent
rmdir "${CGROUP_20}" 2>/dev/null || true
rmdir "${CGROUP_80}" 2>/dev/null || true

echo "=========================================================="
echo "LLM Benchmark Suite Completed Successfully!"
echo "Raw results stored at: ${OUT_DIR}"
echo "=========================================================="
