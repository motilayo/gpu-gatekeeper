#!/usr/bin/env bash
set -euo pipefail

# Ensure running as root for eBPF uprobes and cgroup v2 operations
if [ "$EUID" -ne 0 ]; then
    echo "Re-running test-e2e.sh as root via sudo..."
    exec sudo bash "$0" "$@"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${ROOT_DIR}"

AGENT_PID=""
CGROUP_20="/sys/fs/cgroup/ggate-e2e-20"
CGROUP_80="/sys/fs/cgroup/ggate-e2e-80"
CGROUP_M2="/sys/fs/cgroup/ggate-e2e-m2"
CGROUP_M4="/sys/fs/cgroup/ggate-e2e-m4"
CGROUP_M5="/sys/fs/cgroup/kubepods-pod11111111_2222_3333_4444_555555555555-abcdef123456"
CGROUP_M9="/sys/fs/cgroup/ggate-e2e-m9"
CGROUP_DRA="/sys/fs/cgroup/kubepods-poddra111111_2222_3333_4444_555555555555-abcdef987654"

cleanup() {
    echo "=== Running cleanup ==="
    if [ -n "${AGENT_PID:-}" ] && kill -0 "${AGENT_PID}" 2>/dev/null; then
        echo "Terminating ggate-agent (PID: ${AGENT_PID})..."
        kill -TERM "${AGENT_PID}" 2>/dev/null || true
        wait "${AGENT_PID}" 2>/dev/null || true
    fi
    pkill -9 -f "build/workload_sim" 2>/dev/null || true
    pkill -9 -f "build/ggate-agent" 2>/dev/null || true
    rmdir "${CGROUP_20}" 2>/dev/null || true
    rmdir "${CGROUP_80}" 2>/dev/null || true
    rmdir "${CGROUP_M2}" 2>/dev/null || true
    rmdir "${CGROUP_M4}" 2>/dev/null || true
    rmdir "${CGROUP_M5}" 2>/dev/null || true
    rmdir "${CGROUP_M9}" 2>/dev/null || true
    rmdir "${CGROUP_DRA}" 2>/dev/null || true
    rm -f /tmp/ggate_agent_*.log /tmp/sim_*.log /tmp/mock_kubelet_pods.json /tmp/mock_dra_*.json
}
trap cleanup EXIT

echo "=========================================================="
echo "Milestone 1: Verify mock CUDA library symbol exports"
echo "=========================================================="
if [ ! -f "build/libcuda.so" ]; then
    echo "ERROR: build/libcuda.so not found. Run 'make build-mock' first."
    exit 1
fi

LAUNCH_SYM=$(nm -D build/libcuda.so | grep -w "cuLaunchKernel" || true)
LAUNCH_EX_SYM=$(nm -D build/libcuda.so | grep -w "cuLaunchKernelEx" || true)
GRAPH_SYM=$(nm -D build/libcuda.so | grep -w "cuGraphLaunch" || true)
ALLOC_SYM=$(nm -D build/libcuda.so | grep -w "cuMemAlloc_v2" || true)
FREE_SYM=$(nm -D build/libcuda.so | grep -w "cuMemFree_v2" || true)
ALLOC_ASYNC_SYM=$(nm -D build/libcuda.so | grep -w "cuMemAllocAsync" || true)
FREE_ASYNC_SYM=$(nm -D build/libcuda.so | grep -w "cuMemFreeAsync" || true)

if [ -z "${LAUNCH_SYM}" ]; then
    echo "FAIL: cuLaunchKernel symbol missing from build/libcuda.so"
    exit 1
fi
if [ -z "${LAUNCH_EX_SYM}" ]; then
    echo "FAIL: cuLaunchKernelEx symbol missing from build/libcuda.so"
    exit 1
fi
if [ -z "${GRAPH_SYM}" ]; then
    echo "FAIL: cuGraphLaunch symbol missing from build/libcuda.so"
    exit 1
fi
if [ -z "${ALLOC_SYM}" ]; then
    echo "FAIL: cuMemAlloc_v2 symbol missing from build/libcuda.so"
    exit 1
fi
if [ -z "${FREE_SYM}" ]; then
    echo "FAIL: cuMemFree_v2 symbol missing from build/libcuda.so"
    exit 1
fi
if [ -z "${ALLOC_ASYNC_SYM}" ]; then
    echo "FAIL: cuMemAllocAsync symbol missing from build/libcuda.so"
    exit 1
fi
if [ -z "${FREE_ASYNC_SYM}" ]; then
    echo "FAIL: cuMemFreeAsync symbol missing from build/libcuda.so"
    exit 1
fi

echo "  -> Found: ${LAUNCH_SYM}"
echo "  -> Found: ${LAUNCH_EX_SYM}"
echo "  -> Found: ${GRAPH_SYM}"
echo "  -> Found: ${ALLOC_SYM}"
echo "  -> Found: ${FREE_SYM}"
echo "  -> Found: ${ALLOC_ASYNC_SYM}"
echo "  -> Found: ${FREE_ASYNC_SYM}"
echo "[PASS] Milestone 1 verified successfully."

echo "=========================================================="
echo "Milestone 2: Attribution verification (launches & VRAM)"
echo "=========================================================="
mkdir -p "${CGROUP_M2}"

./build/ggate-agent --libcuda ./build/libcuda.so --metrics-addr :9090 > /tmp/ggate_agent_m2.log 2>&1 &
AGENT_PID=$!

echo "Waiting for ggate-agent to start..."
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
    cat /tmp/ggate_agent_m2.log
    exit 1
fi

echo "Running workload_sim inside cgroup ${CGROUP_M2}..."
OUTPUT=$( (echo $BASHPID > "${CGROUP_M2}/cgroup.procs" && ./build/workload_sim -n 5000 -vram 104857600) )
echo "  -> ${OUTPUT}"

CGROUP_ID=$(echo "${OUTPUT}" | grep -o 'cgroup_id=[0-9]*' | cut -d'=' -f2)
if [ -z "${CGROUP_ID}" ]; then
    echo "FAIL: Could not determine cgroup_id from workload_sim output."
    exit 1
fi
echo "Workload cgroup_id inode: ${CGROUP_ID}"

sleep 0.2
METRICS=$(curl -s http://localhost:9090/metrics)
LAUNCHES=$(echo "${METRICS}" | grep "^ggate_gpu_launches_total{cgroup_id=\"${CGROUP_ID}\".*}" | awk '{print $2}')
VRAM=$(echo "${METRICS}" | grep "^ggate_vram_allocated_bytes{cgroup_id=\"${CGROUP_ID}\".*}" | awk '{print $2}')

echo "Scraped Prometheus metrics for cgroup ${CGROUP_ID}:"
echo "  -> ggate_gpu_launches_total: ${LAUNCHES:-<none>}"
echo "  -> ggate_vram_allocated_bytes: ${VRAM:-<none>}"

if ! awk -v l="${LAUNCHES:-0}" 'BEGIN { exit !(l == 5000) }'; then
    echo "FAIL: Expected ggate_gpu_launches_total == 5000, got '${LAUNCHES}'"
    exit 1
fi

if ! awk -v v="${VRAM:-0}" 'BEGIN { exit !(v == 104857600) }'; then
    echo "FAIL: Expected ggate_vram_allocated_bytes == 104857600, got '${VRAM}'"
    exit 1
fi

echo "[PASS] Milestone 2 verified successfully."

kill -TERM "${AGENT_PID}"
wait "${AGENT_PID}" 2>/dev/null || true
AGENT_PID=""
rmdir "${CGROUP_M2}" 2>/dev/null || true

echo "=========================================================="
echo "Milestone 3: Dynamic cgroup watcher & token-bucket QoS throttling"
echo "=========================================================="
rmdir "${CGROUP_20}" 2>/dev/null || true
rmdir "${CGROUP_80}" 2>/dev/null || true

echo "Starting ggate-agent with dynamic cgroup quota paths..."
./build/ggate-agent \
    --libcuda ./build/libcuda.so \
    --metrics-addr :9090 \
    --quota /sys/fs/cgroup/ggate-e2e-20:6400:64 \
    --quota /sys/fs/cgroup/ggate-e2e-80:25600:256 \
    > /tmp/ggate_agent_m3.log 2>&1 &
AGENT_PID=$!

echo "Waiting for ggate-agent to start..."
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
    cat /tmp/ggate_agent_m3.log
    exit 1
fi

echo "Creating cgroup v2 directories after agent startup..."
mkdir -p "${CGROUP_20}"
mkdir -p "${CGROUP_80}"

sleep 0.2

echo "Running concurrent workload_sim processes (duration=5s)..."
(echo $BASHPID > "${CGROUP_20}/cgroup.procs" && ./build/workload_sim -n 0 -duration 5) > /tmp/sim_20.log 2>&1 &
PID_20=$!

(echo $BASHPID > "${CGROUP_80}/cgroup.procs" && ./build/workload_sim -n 0 -duration 5) > /tmp/sim_80.log 2>&1 &
PID_80=$!

wait "${PID_20}" || { echo "FAIL: Workload 20% slice failed with exit code $?"; cat /tmp/sim_20.log; exit 1; }
wait "${PID_80}" || { echo "FAIL: Workload 80% slice failed with exit code $?"; cat /tmp/sim_80.log; exit 1; }

echo "Workload 20% output:"
cat /tmp/sim_20.log
echo "Workload 80% output:"
cat /tmp/sim_80.log

IOPS_20=$(grep -o 'iops=[0-9.]*' /tmp/sim_20.log | cut -d'=' -f2)
IOPS_80=$(grep -o 'iops=[0-9.]*' /tmp/sim_80.log | cut -d'=' -f2)

echo "Achieved IOPS:"
echo "  -> 20% slice: ${IOPS_20} launches/sec (target: 200.0 ± 5% [190.0, 210.0])"
echo "  -> 80% slice: ${IOPS_80} launches/sec (target: 800.0)"

awk -v iops="${IOPS_20}" 'BEGIN {
    if (iops >= 190.0 && iops <= 210.0) {
        printf("  -> 20%% slice IOPS %.2f is within bounds [190.0, 210.0]\n", iops);
        exit 0;
    } else {
        printf("FAIL: 20%% slice IOPS %.2f is out of bounds [190.0, 210.0]\n", iops);
        exit 1;
    }
}'

awk -v iops20="${IOPS_20}" -v iops80="${IOPS_80}" 'BEGIN {
    ratio = iops20 / iops80;
    min_ratio = 0.25 * 0.95;
    max_ratio = 0.25 * 1.05;
    printf("  -> 20/80 IOPS ratio: %.4f (target 0.2500, bounds [%.4f, %.4f])\n", ratio, min_ratio, max_ratio);
    if (ratio >= min_ratio && ratio <= max_ratio) {
        exit 0;
    } else {
        printf("FAIL: 20/80 IOPS ratio %.4f is out of bounds [%.4f, %.4f]\n", ratio, min_ratio, max_ratio);
        exit 1;
    }
}'

echo "[PASS] Milestone 3 verified successfully."

kill -TERM "${AGENT_PID}"
wait "${AGENT_PID}" 2>/dev/null || true
AGENT_PID=""

echo "=========================================================="
echo "Milestone 4: VRAM lifecycle deallocation tracking (cuMemFree_v2)"
echo "=========================================================="
mkdir -p "${CGROUP_M4}"

./build/ggate-agent --libcuda ./build/libcuda.so --metrics-addr :9090 > /tmp/ggate_agent_m4.log 2>&1 &
AGENT_PID=$!

echo "Waiting for ggate-agent to start..."
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
    cat /tmp/ggate_agent_m4.log
    exit 1
fi

echo "Running workload_sim with -free-on-exit inside cgroup ${CGROUP_M4}..."
OUTPUT=$( (echo $BASHPID > "${CGROUP_M4}/cgroup.procs" && ./build/workload_sim -n 100 -vram 104857600 -free-on-exit) )
echo "  -> ${OUTPUT}"

CGROUP_ID=$(echo "${OUTPUT}" | grep -o 'cgroup_id=[0-9]*' | cut -d'=' -f2)
if [ -z "${CGROUP_ID}" ]; then
    echo "FAIL: Could not determine cgroup_id from workload_sim output."
    exit 1
fi
echo "Workload cgroup_id inode: ${CGROUP_ID}"

sleep 0.2
METRICS=$(curl -s http://localhost:9090/metrics)
LAUNCHES=$(echo "${METRICS}" | grep "^ggate_gpu_launches_total{cgroup_id=\"${CGROUP_ID}\".*}" | awk '{print $2}')
VRAM=$(echo "${METRICS}" | grep "^ggate_vram_allocated_bytes{cgroup_id=\"${CGROUP_ID}\".*}" | awk '{print $2}')

echo "Scraped Prometheus metrics for cgroup ${CGROUP_ID}:"
echo "  -> ggate_gpu_launches_total: ${LAUNCHES:-<none>}"
echo "  -> ggate_vram_allocated_bytes: ${VRAM:-<none>}"

if ! awk -v l="${LAUNCHES:-0}" 'BEGIN { exit !(l == 100) }'; then
    echo "FAIL: Expected ggate_gpu_launches_total == 100, got '${LAUNCHES}'"
    exit 1
fi

if ! awk -v v="${VRAM:-0}" 'BEGIN { exit !(v == 0) }'; then
    echo "FAIL: Expected ggate_vram_allocated_bytes == 0 after cuMemFree_v2, got '${VRAM}'"
    exit 1
fi

echo "[PASS] Milestone 4 verified successfully (100 launches, VRAM deallocated to 0)."

kill -TERM "${AGENT_PID}"
wait "${AGENT_PID}" 2>/dev/null || true
AGENT_PID=""
rmdir "${CGROUP_M4}" 2>/dev/null || true

echo "=========================================================="
echo "Milestone 5: Kubernetes Pod Attribution & Annotation Quota Auto-Discovery"
echo "=========================================================="
rmdir "${CGROUP_M5}" 2>/dev/null || true

cat << 'EOF' > /tmp/mock_kubelet_pods.json
{
  "items": [
    {
      "metadata": {
        "name": "llama3-inference-7b",
        "namespace": "ai-prod",
        "uid": "11111111-2222-3333-4444-555555555555",
        "annotations": {
          "g-gate.io/tokens-per-sec": "6400",
          "g-gate.io/burst-tokens": "64",
          "g-gate.io/max-vram-mb": "32"
        }
      },
      "status": {
        "containerStatuses": [
          {
            "name": "pytorch",
            "containerID": "containerd://abcdef123456"
          }
        ]
      }
    }
  ]
}
EOF

echo "Starting ggate-agent with --kubelet-pods-file (zero CLI quota flags)..."
./build/ggate-agent \
    --libcuda ./build/libcuda.so \
    --metrics-addr :9090 \
    --kubelet-pods-file /tmp/mock_kubelet_pods.json \
    > /tmp/ggate_agent_m5.log 2>&1 &
AGENT_PID=$!

echo "Waiting for ggate-agent to start..."
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
    cat /tmp/ggate_agent_m5.log
    exit 1
fi

echo "Creating Kubernetes Pod container cgroup ${CGROUP_M5}..."
mkdir -p "${CGROUP_M5}"

# Allow resolver to scan cgroup and auto-enroll quota from pod annotations
sleep 0.3

echo "Running workload_sim in Pod cgroup (duration=3s)..."
(echo $BASHPID > "${CGROUP_M5}/cgroup.procs" && ./build/workload_sim -n 0 -duration 3) > /tmp/sim_m5.log 2>&1

cat /tmp/sim_m5.log
IOPS_M5=$(grep -o 'iops=[0-9.]*' /tmp/sim_m5.log | cut -d'=' -f2)
CGROUP_ID_M5=$(grep -o 'cgroup_id=[0-9]*' /tmp/sim_m5.log | cut -d'=' -f2)

echo "Achieved Pod IOPS: ${IOPS_M5} (target: 200.0 from g-gate.io/tokens-per-sec=6400, expected [185.0, 215.0])"

awk -v iops="${IOPS_M5}" 'BEGIN {
    if (iops >= 185.0 && iops <= 215.0) {
        printf("  -> Pod IOPS %.2f is within bounds [185.0, 215.0]\n", iops);
        exit 0;
    } else {
        printf("FAIL: Pod IOPS %.2f is out of bounds [185.0, 215.0]\n", iops);
        exit 1;
    }
}'

sleep 0.2
METRICS=$(curl -s http://localhost:9090/metrics)
echo "Scraped Pod metrics:"
echo "${METRICS}" | grep "llama3-inference-7b" || true

POD_METRIC=$(echo "${METRICS}" | grep 'namespace="ai-prod"' | grep 'pod="llama3-inference-7b"' | grep 'container="pytorch"' || true)
if [ -z "${POD_METRIC}" ]; then
    echo "FAIL: Missing expected Kubernetes labels namespace=ai-prod, pod=llama3-inference-7b, container=pytorch"
    exit 1
fi

THROTTLED_SEC=$(echo "${METRICS}" | grep "^ggate_gpu_throttled_seconds_total{cgroup_id=\"${CGROUP_ID_M5}\".*}" | awk '{print $2}')
echo "Cumulative throttling pause seconds: ${THROTTLED_SEC:-0}"

awk -v sec="${THROTTLED_SEC:-0}" 'BEGIN {
    if (sec > 0) {
        printf("  -> ggate_gpu_throttled_seconds_total %.4f > 0 confirmed\n", sec);
        exit 0;
    } else {
        printf("FAIL: Expected ggate_gpu_throttled_seconds_total > 0, got %s\n", sec);
        exit 1;
    }
}'

VRAM_QUOTA=$(echo "${METRICS}" | grep "^ggate_vram_quota_bytes{cgroup_id=\"${CGROUP_ID_M5}\".*}" | awk '{print $2}')
VRAM_EXCEEDED=$(echo "${METRICS}" | grep "^ggate_vram_quota_exceeded_total{cgroup_id=\"${CGROUP_ID_M5}\".*}" | awk '{print $2}')
echo "Pod VRAM Quota: ${VRAM_QUOTA:-0} (expected 33554432 for 32 MiB)"
echo "Pod VRAM Quota Exceeded count: ${VRAM_EXCEEDED:-0} (expected 1 for 64 MiB alloc)"

if ! awk -v q="${VRAM_QUOTA:-0}" 'BEGIN { exit !(q == 33554432) }'; then
    echo "FAIL: Expected ggate_vram_quota_bytes == 33554432, got '${VRAM_QUOTA}'"
    exit 1
fi

if ! awk -v e="${VRAM_EXCEEDED:-0}" 'BEGIN { exit !(e == 1) }'; then
    echo "FAIL: Expected ggate_vram_quota_exceeded_total == 1, got '${VRAM_EXCEEDED}'"
    exit 1
fi

echo "[PASS] Milestone 5 verified successfully (Pod labels resolved, annotation quota auto-applied, VRAM quota & violation tracked)."

kill -TERM "${AGENT_PID}"
wait "${AGENT_PID}" 2>/dev/null || true
AGENT_PID=""
rmdir "${CGROUP_M5}" 2>/dev/null || true
rm -f /tmp/mock_kubelet_pods.json

echo "=========================================================="
echo "Milestone 9 (v0.2): CUDA Graphs & Stream-Ordered Memory"
echo "=========================================================="
CGROUP_M9="/sys/fs/cgroup/ggate-e2e-m9"
mkdir -p "${CGROUP_M9}"
CGROUP_ID_M9=$(stat -c '%i' "${CGROUP_M9}")

echo "Starting ggate-agent with --libcuda auto and --quota ${CGROUP_M9}:6400:64..."
./build/ggate-agent \
    --libcuda auto \
    --metrics-addr :9090 \
    --quota "${CGROUP_M9}:6400:64" \
    > /tmp/ggate_agent_m9.log 2>&1 &
AGENT_PID=$!

READY=0
for i in {1..50}; do
    if curl -s http://localhost:9090/metrics >/dev/null 2>&1; then
        READY=1
        break
    fi
    sleep 0.1
done
if [ "${READY}" -ne 1 ]; then
    echo "FAIL: ggate-agent failed to start for Milestone 9. Logs:"
    cat /tmp/ggate_agent_m9.log
    exit 1
fi

echo "Running workload_sim with -graph-launch, -async-vram, -free-on-exit inside ${CGROUP_M9}..."
(echo $BASHPID > "${CGROUP_M9}/cgroup.procs" && ./build/workload_sim -n 200 -vram 67108864 -graph-launch -async-vram -free-on-exit) > /tmp/sim_m9.log 2>&1
cat /tmp/sim_m9.log

sleep 0.2
METRICS=$(curl -s http://localhost:9090/metrics)
echo "Scraped Milestone 9 metrics:"
echo "${METRICS}" | grep -E "ggate_(gpu_launches|gpu_throttled|vram)" | grep "cgroup_id=\"${CGROUP_ID_M9}\"" || true

LAUNCHES_M9=$(echo "${METRICS}" | grep "^ggate_gpu_launches_total{cgroup_id=\"${CGROUP_ID_M9}\".*}" | awk '{print $2}')
VRAM_M9=$(echo "${METRICS}" | grep "^ggate_vram_allocated_bytes{cgroup_id=\"${CGROUP_ID_M9}\".*}" | awk '{print $2}')
THROTTLED_SEC_M9=$(echo "${METRICS}" | grep "^ggate_gpu_throttled_seconds_total{cgroup_id=\"${CGROUP_ID_M9}\".*}" | awk '{print $2}')

echo "Launches: ${LAUNCHES_M9:-0} (expected 200)"
echo "Allocated VRAM: ${VRAM_M9:-0} (expected 0 after cuMemFreeAsync)"
echo "Throttled seconds: ${THROTTLED_SEC_M9:-0} (expected > 0)"

if [ "${LAUNCHES_M9}" != "200" ]; then
    echo "FAIL: Expected ggate_gpu_launches_total == 200, got '${LAUNCHES_M9}'"
    exit 1
fi

if [ "${VRAM_M9}" != "0" ]; then
    echo "FAIL: Expected ggate_vram_allocated_bytes == 0 after cuMemFreeAsync, got '${VRAM_M9}'"
    exit 1
fi

if ! awk -v sec="${THROTTLED_SEC_M9:-0}" 'BEGIN { exit !(sec > 0) }'; then
    echo "FAIL: Expected ggate_gpu_throttled_seconds_total > 0, got '${THROTTLED_SEC_M9}'"
    exit 1
fi

echo "[PASS] Milestone 9 (v0.2) verified successfully (cuGraphLaunch, cuMemAllocAsync, cuMemFreeAsync, --libcuda auto)."

kill -TERM "${AGENT_PID}"
wait "${AGENT_PID}" 2>/dev/null || true
AGENT_PID=""
rmdir "${CGROUP_M9}" 2>/dev/null || true

echo "=========================================================="
echo "Milestone 10 (v0.3): Native Kubernetes DRA (resource.k8s.io/v1) ResourceClaim Enforcement"
echo "=========================================================="
rmdir "${CGROUP_DRA}" 2>/dev/null || true

cat << 'EOF' > /tmp/mock_dra_pods.json
{
  "items": [
    {
      "metadata": {
        "name": "vllm-dra-pod",
        "namespace": "ai-prod",
        "uid": "dra111111-2222-3333-4444-555555555555",
        "annotations": {}
      },
      "status": {
        "containerStatuses": [
          {
            "name": "vllm",
            "containerID": "containerd://abcdef987654"
          }
        ],
        "resourceClaimStatuses": [
          {
            "name": "gpu",
            "resourceClaimName": "vllm-dra-claim-01"
          }
        ]
      }
    }
  ]
}
EOF

cat << 'EOF' > /tmp/mock_dra_claims.json
{
  "items": [
    {
      "metadata": {
        "name": "vllm-dra-claim-01",
        "namespace": "ai-prod",
        "uid": "dra-claim-uid-01"
      },
      "status": {
        "allocation": {
          "devices": {
            "results": [
              {
                "request": "gpu",
                "driver": "gpu.g-gate.io",
                "consumedCapacity": {
                  "gpu.g-gate.io/tokens-per-sec": "6400",
                  "gpu.g-gate.io/burst-tokens": "64",
                  "memory": "32Mi"
                }
              }
            ]
          }
        }
      }
    }
  ]
}
EOF

echo "Starting ggate-agent with DRA claims file, CDI dir, and zero Pod annotations..."
rm -rf /tmp/ggate_cdi_test
./build/ggate-agent \
    --libcuda ./build/libcuda.so \
    --metrics-addr :9090 \
    --kubelet-pods-file /tmp/mock_dra_pods.json \
    --dra-claims-file /tmp/mock_dra_claims.json \
    --cdi-dir /tmp/ggate_cdi_test \
    > /tmp/ggate_agent_dra.log 2>&1 &
AGENT_PID=$!

echo "Waiting for ggate-agent to start..."
READY=0
for i in {1..50}; do
    if curl -s http://localhost:9090/metrics >/dev/null 2>&1; then
        READY=1
        break
    fi
    sleep 0.1
done
if [ "${READY}" -ne 1 ]; then
    echo "FAIL: ggate-agent failed to start for DRA milestone. Logs:"
    cat /tmp/ggate_agent_dra.log
    exit 1
fi

echo "Creating Kubernetes Pod container cgroup ${CGROUP_DRA}..."
mkdir -p "${CGROUP_DRA}"

# Allow resolver to scan cgroup and auto-enroll quota from DRA ResourceClaim
sleep 0.3

echo "Running workload_sim in DRA Pod cgroup (duration=3s, vram=64MiB against 32MiB claim limit)..."
(echo $BASHPID > "${CGROUP_DRA}/cgroup.procs" && ./build/workload_sim -n 0 -duration 3) > /tmp/sim_dra.log 2>&1

cat /tmp/sim_dra.log
IOPS_DRA=$(grep -o 'iops=[0-9.]*' /tmp/sim_dra.log | cut -d'=' -f2)
CGROUP_ID_DRA=$(grep -o 'cgroup_id=[0-9]*' /tmp/sim_dra.log | cut -d'=' -f2)

echo "Achieved DRA Pod IOPS: ${IOPS_DRA} (target: 200.0 from DRA tokens-per-sec=6400, expected [185.0, 215.0])"

awk -v iops="${IOPS_DRA}" 'BEGIN {
    if (iops >= 185.0 && iops <= 215.0) {
        printf("  -> DRA Pod IOPS %.2f is within bounds [185.0, 215.0]\n", iops);
        exit 0;
    } else {
        printf("FAIL: DRA Pod IOPS %.2f is out of bounds [185.0, 215.0]\n", iops);
        exit 1;
    }
}'

sleep 0.2
METRICS=$(curl -s http://localhost:9090/metrics)
echo "Scraped DRA Pod metrics:"
echo "${METRICS}" | grep "vllm-dra-pod" || true

POD_METRIC=$(echo "${METRICS}" | grep 'namespace="ai-prod"' | grep 'pod="vllm-dra-pod"' | grep 'container="vllm"' | grep 'resource_claim="vllm-dra-claim-01"' || true)
if [ -z "${POD_METRIC}" ]; then
    echo "FAIL: Missing expected Kubernetes labels namespace=ai-prod, pod=vllm-dra-pod, container=vllm, resource_claim=vllm-dra-claim-01"
    exit 1
fi

VRAM_QUOTA_DRA=$(echo "${METRICS}" | grep "^ggate_vram_quota_bytes{cgroup_id=\"${CGROUP_ID_DRA}\".*}" | awk '{print $2}')
VRAM_EXCEEDED_DRA=$(echo "${METRICS}" | grep "^ggate_vram_quota_exceeded_total{cgroup_id=\"${CGROUP_ID_DRA}\".*}" | awk '{print $2}')
echo "DRA Pod VRAM Quota: ${VRAM_QUOTA_DRA:-0} (expected 33554432 for 32Mi claim)"
echo "DRA Pod VRAM Quota Exceeded count: ${VRAM_EXCEEDED_DRA:-0} (expected 1 for 64 MiB alloc)"

if ! awk -v q="${VRAM_QUOTA_DRA:-0}" 'BEGIN { exit !(q == 33554432) }'; then
    echo "FAIL: Expected ggate_vram_quota_bytes == 33554432, got '${VRAM_QUOTA_DRA}'"
    exit 1
fi

if ! awk -v e="${VRAM_EXCEEDED_DRA:-0}" 'BEGIN { exit !(e == 1) }'; then
    echo "FAIL: Expected ggate_vram_quota_exceeded_total == 1, got '${VRAM_EXCEEDED_DRA}'"
    exit 1
fi

if [ ! -f "/tmp/ggate_cdi_test/gpu.g-gate.io.json" ]; then
    echo "FAIL: Expected CDI 0.6.0 specification file at /tmp/ggate_cdi_test/gpu.g-gate.io.json"
    exit 1
fi
if ! grep -q "claim-dra-claim-uid-01" /tmp/ggate_cdi_test/gpu.g-gate.io.json; then
    echo "FAIL: Expected claim-dra-claim-uid-01 in /tmp/ggate_cdi_test/gpu.g-gate.io.json"
    cat /tmp/ggate_cdi_test/gpu.g-gate.io.json
    exit 1
fi
if ! grep -q "GGATE_RESOURCE_CLAIM=vllm-dra-claim-01" /tmp/ggate_cdi_test/gpu.g-gate.io.json; then
    echo "FAIL: Expected GGATE_RESOURCE_CLAIM=vllm-dra-claim-01 in /tmp/ggate_cdi_test/gpu.g-gate.io.json"
    cat /tmp/ggate_cdi_test/gpu.g-gate.io.json
    exit 1
fi
if ! grep -q "GGATE_CLAIM_UID=dra-claim-uid-01" /tmp/ggate_cdi_test/gpu.g-gate.io.json; then
    echo "FAIL: Expected GGATE_CLAIM_UID=dra-claim-uid-01 in /tmp/ggate_cdi_test/gpu.g-gate.io.json"
    cat /tmp/ggate_cdi_test/gpu.g-gate.io.json
    exit 1
fi
echo "Verified CDI 0.6.0 spec generated at /tmp/ggate_cdi_test/gpu.g-gate.io.json:"
cat /tmp/ggate_cdi_test/gpu.g-gate.io.json
rm -rf /tmp/ggate_cdi_test

echo "[PASS] Milestone 10 (v0.3) verified successfully (Kubernetes DRA ResourceClaim quota enforcement + CDI 0.6.0 spec generation with ZERO annotations)."

kill -TERM "${AGENT_PID}"
wait "${AGENT_PID}" 2>/dev/null || true
AGENT_PID=""
rmdir "${CGROUP_DRA}" 2>/dev/null || true

echo "=========================================================="
echo "ALL MILESTONES (1, 2, 3, 4, 5, 9, 10) VERIFIED SUCCESSFULLY!"
echo "=========================================================="
