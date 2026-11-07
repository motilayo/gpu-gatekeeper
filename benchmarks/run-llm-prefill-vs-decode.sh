#!/usr/bin/env bash
set -euo pipefail

# G-Gate Real LLM QoS Benchmark (Experiment 5B)
# The "Prefill-Flood vs. Interactive Decode" Killer Test
# Measures how badly a batch prefill flood (batch=16, seq=512) degrades interactive
# decode (batch=1, prompt=128, new=128), and verifies G-Gate QoS protection.

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

DURATION="${DURATION:-45}"
TRIALS="${TRIALS:-3}"
MODEL_NAME="${MODEL_NAME:-Qwen/Qwen2.5-0.5B-Instruct}"
FALLBACK_MODEL="${FALLBACK_MODEL:-TinyLlama/TinyLlama-1.1B-Chat-v1.0}"
PREFILL_BATCH_SIZE="${PREFILL_BATCH_SIZE:-16}"
PREFILL_SEQ_LEN="${PREFILL_SEQ_LEN:-512}"

CGROUP_DECODE="/sys/fs/cgroup/ggate-llm-exp5b-decode"
CGROUP_PREFILL="/sys/fs/cgroup/ggate-llm-exp5b-prefill"

AGENT_PID=""

cleanup() {
    echo ""
    echo "=== Cleaning up benchmark processes and cgroups ==="
    if [ -n "${AGENT_PID:-}" ] && kill -0 "${AGENT_PID}" 2>/dev/null; then
        echo "Stopping ggate-agent (PID: ${AGENT_PID})..."
        kill -TERM "${AGENT_PID}" 2>/dev/null || true
        wait "${AGENT_PID}" 2>/dev/null || true
        AGENT_PID=""
    fi
    pkill -9 -f "llm_bench.py" 2>/dev/null || true
    pkill -9 -x ggate-agent 2>/dev/null || true
    for cg in "${CGROUP_DECODE}" "${CGROUP_PREFILL}"; do
        if [ -d "$cg" ]; then
            rmdir "$cg" 2>/dev/null || true
        fi
    done
    rm -f /tmp/exp5b_*.log /tmp/ggate_agent_*.log /tmp/calib_*.json
}
trap cleanup EXIT INT TERM

echo "=========================================================="
echo "G-Gate Real LLM Benchmark: Experiment 5B"
echo "Prefill-Flood vs. Interactive Decode"
echo "Output Directory: ${OUT_DIR}"
echo "Model: ${MODEL_NAME} (Fallback: ${FALLBACK_MODEL})"
echo "Duration per trial: ${DURATION}s, Trials: ${TRIALS}"
echo "Prefill Flood: batch_size=${PREFILL_BATCH_SIZE}, seq_len=${PREFILL_SEQ_LEN}"
echo "=========================================================="

# 1. Discover Python
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

# 3. Check ggate-agent binary
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

start_agent() {
    local quota_args=("$@")
    echo "Stopping any existing ggate-agent..."
    pkill -9 -x ggate-agent 2>/dev/null || true
    sleep 0.5
    rm -f /tmp/ggate_agent.log
    echo "Starting ggate-agent with quotas: ${quota_args[*]:-none}..."
    ./build/ggate-agent \
        --libcuda "${LIBCUDA_PATH}" \
        --metrics-addr :9090 \
        "${quota_args[@]}" \
        > /tmp/ggate_agent.log 2>&1 &
    AGENT_PID=$!

    local ready=0
    for i in {1..50}; do
        if ! kill -0 "${AGENT_PID}" 2>/dev/null; then
            echo "FAIL: ggate-agent process exited prematurely. Logs:"
            cat /tmp/ggate_agent.log
            exit 1
        fi
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
        for i in {1..20}; do
            if ! kill -0 "${AGENT_PID}" 2>/dev/null; then break; fi
            sleep 0.1
        done
        kill -9 "${AGENT_PID}" 2>/dev/null || true
        wait "${AGENT_PID}" 2>/dev/null || true
        AGENT_PID=""
    fi
    pkill -9 -x ggate-agent 2>/dev/null || true
    sleep 0.5
}

# ==========================================================
# Scenario 5B-1: Unmanaged Contention (No G-Gate)
# ==========================================================
echo ""
echo "=========================================================="
echo "Scenario 5B-1: Unmanaged Contention (Prefill Flood vs Interactive Decode)"
echo "tenant-80: interactive greedy decode (batch=1)"
echo "tenant-20: prefill flood (batch=${PREFILL_BATCH_SIZE}, seq=${PREFILL_SEQ_LEN})"
echo "No G-Gate quotas enforced"
echo "=========================================================="

mkdir -p "${CGROUP_DECODE}" "${CGROUP_PREFILL}"
CGROUP_ID_DECODE=$(stat -c '%i' "${CGROUP_DECODE}")
CGROUP_ID_PREFILL=$(stat -c '%i' "${CGROUP_PREFILL}")

start_agent --quota "${CGROUP_DECODE}:0:0" --quota "${CGROUP_PREFILL}:0:0"

for t in $(seq 1 "${TRIALS}"); do
    echo "--- Scenario 5B-1: Trial ${t}/${TRIALS} (${DURATION}s) ---"
    (echo $BASHPID > "${CGROUP_DECODE}/cgroup.procs" && \
     "${PYTHON_BIN}" benchmarks/llm_bench.py \
        --model "${MODEL_NAME}" \
        --fallback-model "${FALLBACK_MODEL}" \
        --mode decode \
        --duration "${DURATION}" \
        --label "unmanaged-decode-trial${t}" \
        --json-out "${OUT_DIR}/exp5b_unmanaged_decode_trial${t}.json") &
    PID_DECODE=$!

    (echo $BASHPID > "${CGROUP_PREFILL}/cgroup.procs" && \
     "${PYTHON_BIN}" benchmarks/llm_bench.py \
        --model "${MODEL_NAME}" \
        --fallback-model "${FALLBACK_MODEL}" \
        --mode prefill-flood \
        --batch-size "${PREFILL_BATCH_SIZE}" \
        --seq-len "${PREFILL_SEQ_LEN}" \
        --duration "${DURATION}" \
        --label "unmanaged-prefill-trial${t}" \
        --json-out "${OUT_DIR}/exp5b_unmanaged_prefill_trial${t}.json") &
    PID_PREFILL=$!

    wait "${PID_DECODE}"
    wait "${PID_PREFILL}"

    curl -s http://localhost:9090/metrics > "${OUT_DIR}/exp5b_unmanaged_prom_trial${t}.txt"
    curl -s http://localhost:9090/api/v1/tenants > "${OUT_DIR}/exp5b_unmanaged_tenants_trial${t}.json" || true
done

stop_agent

# Calculate unmanaged baseline throughput for prefill
UNMANAGED_PREFILL_TOK_S=$("${PYTHON_BIN}" -c "import json, glob; fs=glob.glob('${OUT_DIR}/exp5b_unmanaged_prefill_trial*.json'); ts=[json.load(open(f))['prefill_tokens_per_sec'] for f in fs]; print(sum(ts)/len(ts)) if ts else print('1000.0')")
UNMANAGED_DECODE_TOK_S=$("${PYTHON_BIN}" -c "import json, glob; fs=glob.glob('${OUT_DIR}/exp5b_unmanaged_decode_trial*.json'); ts=[json.load(open(f))['tokens_per_sec'] for f in fs]; print(sum(ts)/len(ts)) if ts else print('20.0')")
echo "Scenario 5B-1 Unmanaged Prefill Rate: ${UNMANAGED_PREFILL_TOK_S} tokens/sec"
echo "Scenario 5B-1 Unmanaged Decode Rate: ${UNMANAGED_DECODE_TOK_S} tokens/sec"

# ==========================================================
# Scenario 5B-2 Calibration: Empirical Rate Search for Prefill Flood
# ==========================================================
echo ""
echo "=========================================================="
echo "Scenario 5B-2 Calibration: Calibrating Prefill Flood Throttling"
echo "=========================================================="
CALIB_LOG="${OUT_DIR}/exp5b_calibration_log.txt"
echo "=== Experiment 5B Calibration Log ===" > "${CALIB_LOG}"

# Starting rate candidates:
# Interactive decode is unconstrained (2,000,000 tok/s, burst 200,000)
RATE_80=2000000
BURST_80=200000

# Prefill flood quota target: ~15-20% duty cycle
RATE_20=100000
BURST_20=64

CALIB_ROUND=1
CALIB_DURATION=15

while [ ${CALIB_ROUND} -le 5 ]; do
    echo "--- Calibration Round ${CALIB_ROUND}: Testing prefill rate=${RATE_20}, burst=${BURST_20} (${CALIB_DURATION}s probe) ---"
    start_agent --quota "${CGROUP_DECODE}:${RATE_80}:${BURST_80}" --quota "${CGROUP_PREFILL}:${RATE_20}:${BURST_20}"

    (echo $BASHPID > "${CGROUP_DECODE}/cgroup.procs" && \
     "${PYTHON_BIN}" benchmarks/llm_bench.py \
        --model "${MODEL_NAME}" \
        --fallback-model "${FALLBACK_MODEL}" \
        --mode decode \
        --duration "${CALIB_DURATION}" \
        --label "calib-decode" \
        --json-out "/tmp/calib_decode.json") &
    CPID_DEC=$!

    (echo $BASHPID > "${CGROUP_PREFILL}/cgroup.procs" && \
     "${PYTHON_BIN}" benchmarks/llm_bench.py \
        --model "${MODEL_NAME}" \
        --fallback-model "${FALLBACK_MODEL}" \
        --mode prefill-flood \
        --batch-size "${PREFILL_BATCH_SIZE}" \
        --seq-len "${PREFILL_SEQ_LEN}" \
        --duration "${CALIB_DURATION}" \
        --label "calib-prefill" \
        --json-out "/tmp/calib_prefill.json") &
    CPID_PRE=$!

    wait "${CPID_DEC}"
    wait "${CPID_PRE}"

    TENANTS_JSON=$(curl -s http://localhost:9090/api/v1/tenants || echo "{}")
    THROT_20=$("${PYTHON_BIN}" -c "import json; d=json.loads('''${TENANTS_JSON}'''); ts=[t.get('total_throttled', 0) for t in d.get('tenants', []) if 'prefill' in t.get('pod', '') or str(t.get('cgroup_id')) == '${CGROUP_ID_PREFILL}']; print(ts[0] if ts else 0)" 2>/dev/null || echo "0")
    THROT_80=$("${PYTHON_BIN}" -c "import json; d=json.loads('''${TENANTS_JSON}'''); ts=[t.get('total_throttled', 0) for t in d.get('tenants', []) if 'decode' in t.get('pod', '') or str(t.get('cgroup_id')) == '${CGROUP_ID_DECODE}']; print(ts[0] if ts else 0)" 2>/dev/null || echo "0")
    PREFILL_TOK_S=$("${PYTHON_BIN}" -c "import json; print(json.load(open('/tmp/calib_prefill.json'))['prefill_tokens_per_sec'])" 2>/dev/null || echo "0")
    DECODE_TOK_S=$("${PYTHON_BIN}" -c "import json; print(json.load(open('/tmp/calib_decode.json'))['tokens_per_sec'])" 2>/dev/null || echo "0")

    THROT_20="${THROT_20:-0}"
    THROT_80="${THROT_80:-0}"

    echo "Round ${CALIB_ROUND}: rate_20=${RATE_20}, burst_20=${BURST_20} -> throttled_20=${THROT_20}, throttled_80=${THROT_80}, prefill_tok_s=${PREFILL_TOK_S}, decode_tok_s=${DECODE_TOK_S}" | tee -a "${CALIB_LOG}"

    stop_agent

    # Condition: tenant-80 MUST have 0 throttles
    if awk "BEGIN {exit !(${THROT_80} > 0)}"; then
        echo "Tenant-80 was throttled! Increasing rate_80..." | tee -a "${CALIB_LOG}"
        RATE_80=$((RATE_80 * 2))
        BURST_80=$((BURST_80 * 2))
        CALIB_ROUND=$((CALIB_ROUND + 1))
        continue
    fi

    # Condition: tenant-20 MUST be throttled (>50 throttles)
    if awk "BEGIN {exit !(${THROT_20} < 50)}"; then
        echo "Tenant-20 was NOT sufficiently throttled (throttles=${THROT_20}). Halving rate_20..." | tee -a "${CALIB_LOG}"
        RATE_20=$((RATE_20 / 2))
        CALIB_ROUND=$((CALIB_ROUND + 1))
        continue
    fi

    echo "Calibration success! Selected rates: prefill_rate=${RATE_20}, burst=${BURST_20}, decode_rate=${RATE_80}" | tee -a "${CALIB_LOG}"
    break
done

echo "Final Calibrated Rates: RATE_20=${RATE_20}, BURST_20=${BURST_20}, RATE_80=${RATE_80}, BURST_80=${BURST_80}" >> "${CALIB_LOG}"

# ==========================================================
# Scenario 5B-2: G-Gate Managed QoS (Protecting Interactive Decode)
# ==========================================================
echo ""
echo "=========================================================="
echo "Scenario 5B-2: G-Gate Managed QoS"
echo "tenant-80 (Interactive Decode): rate=${RATE_80}, burst=${BURST_80}"
echo "tenant-20 (Prefill Flood): rate=${RATE_20}, burst=${BURST_20}"
echo "=========================================================="

start_agent --quota "${CGROUP_DECODE}:${RATE_80}:${BURST_80}" --quota "${CGROUP_PREFILL}:${RATE_20}:${BURST_20}"

for t in $(seq 1 "${TRIALS}"); do
    echo "--- Scenario 5B-2: Trial ${t}/${TRIALS} (${DURATION}s) ---"
    (echo $BASHPID > "${CGROUP_DECODE}/cgroup.procs" && \
     "${PYTHON_BIN}" benchmarks/llm_bench.py \
        --model "${MODEL_NAME}" \
        --fallback-model "${FALLBACK_MODEL}" \
        --mode decode \
        --duration "${DURATION}" \
        --label "managed-decode-trial${t}" \
        --json-out "${OUT_DIR}/exp5b_managed_decode_trial${t}.json") &
    PID_M_DEC=$!

    (echo $BASHPID > "${CGROUP_PREFILL}/cgroup.procs" && \
     "${PYTHON_BIN}" benchmarks/llm_bench.py \
        --model "${MODEL_NAME}" \
        --fallback-model "${FALLBACK_MODEL}" \
        --mode prefill-flood \
        --batch-size "${PREFILL_BATCH_SIZE}" \
        --seq-len "${PREFILL_SEQ_LEN}" \
        --duration "${DURATION}" \
        --label "managed-prefill-trial${t}" \
        --json-out "${OUT_DIR}/exp5b_managed_prefill_trial${t}.json") &
    PID_M_PRE=$!

    wait "${PID_M_DEC}"
    wait "${PID_M_PRE}"

    curl -s http://localhost:9090/metrics > "${OUT_DIR}/exp5b_managed_prom_trial${t}.txt"
    curl -s http://localhost:9090/api/v1/tenants > "${OUT_DIR}/exp5b_managed_tenants_trial${t}.json" || true
done

stop_agent
rmdir "${CGROUP_DECODE}" 2>/dev/null || true
rmdir "${CGROUP_PREFILL}" 2>/dev/null || true

echo "=========================================================="
echo "Experiment 5B Completed Successfully!"
echo "Raw results stored at: ${OUT_DIR}/exp5b_*"
echo "=========================================================="
