#!/usr/bin/env bash
set -euo pipefail

# Milestone 8: Live k3s E2E Verification Script
# Verifies Kubelet HTTPS :10250/pods Bearer token resolution,
# Pod annotation token-bucket QoS throttling (~200 IOPS),
# and Per-Pod VRAM quota violation detection.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

if [ "$(id -u)" -ne 0 ]; then
    echo "Re-running test-k3s.sh as root via sudo..."
    exec sudo bash "$0" "$@"
fi

echo "=========================================================="
echo " Starting Milestone 8: Live k3s E2E Verification"
echo "=========================================================="

AGENT_PID=""
TOKEN_FILE="/tmp/ggate_sa.token"

cleanup() {
    echo ""
    echo "==> Cleaning up test resources..."
    if [ -n "$AGENT_PID" ] && kill -0 "$AGENT_PID" 2>/dev/null; then
        echo "Stopping ggate-agent (pid=$AGENT_PID)..."
        kill -SIGINT "$AGENT_PID" 2>/dev/null || true
        wait "$AGENT_PID" 2>/dev/null || true
    fi
    if command -v k3s >/dev/null 2>&1; then
        echo "Deleting test pod llama3-k3s-test..."
        k3s kubectl delete pod llama3-k3s-test -n default --ignore-not-found=true --grace-period=0 --force 2>/dev/null || true
    fi
    rm -f "$TOKEN_FILE" /tmp/ggate_agent_k3s.log
    echo "Cleanup complete."
}
trap cleanup EXIT INT TERM

# Ensure build artifacts exist
if [ ! -f "build/ggate-agent" ] || [ ! -f "build/libcuda.so" ] || [ ! -f "build/workload_sim" ]; then
    echo "Error: Build artifacts missing in build/. Run 'make all' first." >&2
    exit 1
fi
chmod -R a+rx "$ROOT_DIR/build"

# 1. Install k3s if not present and start service
if ! command -v k3s >/dev/null 2>&1; then
    echo "==> k3s not found. Installing k3s..."
    curl -sfL https://get.k3s.io | sh -
fi

if ! systemctl is-active --quiet k3s 2>/dev/null; then
    echo "==> Starting k3s service..."
    systemctl start k3s
fi

echo "==> Waiting for k3s node to be Ready..."
READY=0
for i in $(seq 1 60); do
    if k3s kubectl get nodes 2>/dev/null | grep -q "\bReady\b"; then
        echo "k3s node is Ready."
        READY=1
        break
    fi
    sleep 2
done
if [ "$READY" -ne 1 ]; then
    echo "Error: Timed out waiting for k3s node to become Ready." >&2
    exit 1
fi

echo "==> Waiting for default ServiceAccount in namespace 'default'..."
for i in $(seq 1 30); do
    if k3s kubectl get serviceaccount default -n default >/dev/null 2>&1; then
        echo "Default ServiceAccount is ready."
        break
    fi
    sleep 1
done

# 2. Apply ggate RBAC (ServiceAccount, ClusterRole, ClusterRoleBinding)
echo "==> Applying RBAC for ggate-agent..."
k3s kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ggate-agent
  namespace: kube-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ggate-agent
rules:
  - apiGroups: [""]
    resources: ["nodes", "nodes/proxy", "pods"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["resource.k8s.io"]
    resources: ["resourceclaims", "resourceclaimtemplates", "deviceclasses"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["resource.k8s.io"]
    resources: ["resourceslices"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ggate-agent
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ggate-agent
subjects:
  - kind: ServiceAccount
    name: ggate-agent
    namespace: kube-system
EOF

# 3. Create ServiceAccount Bearer token for Kubelet HTTPS :10250 access
echo "==> Generating ServiceAccount Bearer token..."
k3s kubectl create token ggate-agent -n kube-system --duration=24h > "$TOKEN_FILE"
chmod 600 "$TOKEN_FILE"

echo "==> Verifying Kubelet HTTPS :10250/pods connectivity with Bearer token..."
KUBELET_HTTP_CODE=$(curl -sk -o /dev/null -w "%{http_code}" \
    -H "Authorization: Bearer $(cat "$TOKEN_FILE")" \
    https://127.0.0.1:10250/pods || true)
echo "Kubelet HTTPS :10250/pods returned HTTP status: ${KUBELET_HTTP_CODE}"
if [ "$KUBELET_HTTP_CODE" != "200" ]; then
    echo "Warning: Kubelet returned HTTP $KUBELET_HTTP_CODE, continuing to test..."
fi

# Clean up any existing test pod
k3s kubectl delete pod llama3-k3s-test -n default --ignore-not-found=true --grace-period=0 --force 2>/dev/null || true

# 4. Start host ggate-agent
echo "==> Starting ggate-agent with Kubelet HTTPS (:10250/pods) integration..."
./build/ggate-agent \
    --libcuda ./build/libcuda.so \
    --kubelet-pods-url https://127.0.0.1:10250/pods \
    --kubelet-token-file "$TOKEN_FILE" \
    --metrics-addr :9090 > /tmp/ggate_agent_k3s.log 2>&1 &
AGENT_PID=$!

for i in $(seq 1 30); do
    if curl -s http://localhost:9090/metrics >/dev/null 2>&1; then
        echo "ggate-agent is listening on :9090 (pid=$AGENT_PID)"
        break
    fi
    if ! kill -0 "$AGENT_PID" 2>/dev/null; then
        echo "Error: ggate-agent failed to start. Logs:" >&2
        cat /tmp/ggate_agent_k3s.log
        exit 1
    fi
    sleep 0.2
done

# 5. Deploy annotated Kubernetes test Pod
echo "==> Deploying test Pod llama3-k3s-test into namespace 'default'..."
k3s kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: llama3-k3s-test
  namespace: default
  annotations:
    g-gate.io/tokens-per-sec: "6400"
    g-gate.io/burst-tokens: "64"
    g-gate.io/max-vram-mb: "32"
spec:
  restartPolicy: Never
  containers:
    - name: workload
      image: ubuntu:24.04
      command: ["/bin/sh", "-c", "sleep 1 && exec /build/workload_sim -n 0 -duration 5"]
      volumeMounts:
        - name: host-build
          mountPath: /build
  volumes:
    - name: host-build
      hostPath:
        path: ${ROOT_DIR}/build
EOF

# 6. Wait for Pod completion
echo "==> Waiting for llama3-k3s-test to complete (up to 120s)..."
SUCCEEDED=0
for i in $(seq 1 120); do
    PHASE=$(k3s kubectl get pod llama3-k3s-test -n default -o jsonpath='{.status.phase}' 2>/dev/null || true)
    if [ "$PHASE" = "Succeeded" ]; then
        echo "Pod llama3-k3s-test succeeded after ~${i}s."
        SUCCEEDED=1
        break
    elif [ "$PHASE" = "Failed" ]; then
        echo "Error: Pod llama3-k3s-test entered Failed state!" >&2
        k3s kubectl describe pod llama3-k3s-test -n default || true
        k3s kubectl logs llama3-k3s-test -n default || true
        exit 1
    fi
    sleep 1
done

if [ "$SUCCEEDED" -ne 1 ]; then
    echo "Error: Timed out waiting for pod llama3-k3s-test." >&2
    k3s kubectl describe pod llama3-k3s-test -n default || true
    k3s kubectl logs llama3-k3s-test -n default || true
    exit 1
fi

echo "==> Pod Logs:"
POD_LOGS=$(k3s kubectl logs llama3-k3s-test -n default)
echo "$POD_LOGS"

# Allow metrics collector a brief moment to scrape latest counters
sleep 1

echo "==> Scraping Prometheus metrics from ggate-agent..."
METRICS=$(curl -s http://localhost:9090/metrics)
echo "$METRICS" | grep -E "ggate_(gpu_launches|gpu_throttled|vram)" || true

echo ""
echo "=========================================================="
echo " Verifying Assertions"
echo "=========================================================="

# Assertion 1: Live Kubelet HTTPS :10250/pods resolved namespace="default",pod="llama3-k3s-test"
echo "Checking Assertion 1: Pod attribution via Kubelet HTTPS..."
if echo "$METRICS" | grep -q 'pod="llama3-k3s-test"'; then
    echo "  [PASS] Pod identity attributed: pod=\"llama3-k3s-test\", namespace=\"default\""
else
    echo "  [FAIL] pod=\"llama3-k3s-test\" not found in metrics." >&2
    echo "Agent logs:"
    cat /tmp/ggate_agent_k3s.log
    exit 1
fi

# Assertion 2: Pod throttled to ~200 IOPS ([175.0, 225.0])
echo "Checking Assertion 2: QoS throttling governor enforcement..."
IOPS=$(echo "$POD_LOGS" | grep -o 'iops=[0-9.]*' | cut -d= -f2 || true)
echo "  Measured IOPS: ${IOPS:-unknown}"
if [ -z "$IOPS" ]; then
    echo "  [FAIL] Could not extract IOPS from pod logs." >&2
    exit 1
fi
AWK_CHECK=$(awk -v iops="$IOPS" 'BEGIN { if (iops >= 175.0 && iops <= 225.0) print "ok"; else print "fail" }')
if [ "$AWK_CHECK" = "ok" ]; then
    echo "  [PASS] Pod throttled to ~200 IOPS (measured ${IOPS} in [175.0, 225.0])"
else
    echo "  [FAIL] Expected IOPS in [175.0, 225.0], got ${IOPS}" >&2
    exit 1
fi

# Check that ggate_gpu_throttled_total is positive
THROTTLED=$(echo "$METRICS" | grep 'ggate_gpu_throttled_total' | grep 'pod="llama3-k3s-test"' | awk '{sum += $2} END {print sum}')
if [ -n "$THROTTLED" ] && awk -v t="$THROTTLED" 'BEGIN { exit !(t > 0) }'; then
    echo "  [PASS] ggate_gpu_throttled_total > 0 (count: $THROTTLED)"
else
    echo "  [FAIL] Expected ggate_gpu_throttled_total > 0, got ${THROTTLED:-0}" >&2
    exit 1
fi

# Assertion 3: ggate_vram_quota_exceeded_total == 1
echo "Checking Assertion 3: Per-Pod VRAM quota violation detection..."
VRAM_EXCEEDED=$(echo "$METRICS" | grep 'ggate_vram_quota_exceeded_total' | grep 'pod="llama3-k3s-test"' | awk '{sum += $2} END {print sum}')
echo "  Measured ggate_vram_quota_exceeded_total: ${VRAM_EXCEEDED:-0}"
if [ "$VRAM_EXCEEDED" = "1" ]; then
    echo "  [PASS] ggate_vram_quota_exceeded_total == 1 (32 MiB quota exceeded by 64 MiB allocation)"
else
    echo "  [FAIL] Expected ggate_vram_quota_exceeded_total == 1, got ${VRAM_EXCEEDED:-0}" >&2
    exit 1
fi

echo ""
echo "=========================================================="
echo " ALL MILESTONE 8 K3S ASSERTIONS PASSED!"
echo "=========================================================="
