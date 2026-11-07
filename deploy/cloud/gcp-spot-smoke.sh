#!/usr/bin/env bash
set -euo pipefail

if [ -d "${HOME}/google-cloud-sdk/bin" ]; then
    export PATH="${HOME}/google-cloud-sdk/bin:${PATH}"
fi
if [ -z "${CLOUDSDK_PYTHON:-}" ] && [ -x "/opt/homebrew/bin/python3.11" ]; then
    export CLOUDSDK_PYTHON="/opt/homebrew/bin/python3.11"
fi

# GCP Spot Smoke-Test for G-Gate Real GPU Validation
# Defaults: g2-standard-4 (NVIDIA L4 GPU) Spot (~$0.22/hr) in us-central1-a
# with built-in GCP 90-minute hard termination dead-man's switch (--max-run-duration=90m)

MACHINE_TYPE="${MACHINE_TYPE:-g2-standard-4}"
ZONE="${GCP_ZONE:-${ZONE:-us-central1-a}}"
INSTANCE_NAME="ggate-spot-smoke"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"

check_auth() {
    if ! command -v gcloud >/dev/null 2>&1; then
        echo "Error: gcloud CLI is not installed or not in PATH."
        exit 1
    fi
    ACTIVE_ACCT=$(gcloud auth list --filter=status:ACTIVE --format="value(account)" 2>/dev/null || true)
    if [ -z "${ACTIVE_ACCT}" ]; then
        echo "Error: No active gcloud authenticated account found."
        echo "Please log in using: gcloud auth login"
        exit 1
    fi
}

instance_exists() {
    gcloud compute instances describe "${INSTANCE_NAME}" --zone="${ZONE}" >/dev/null 2>&1
}

cmd_up() {
    check_auth
    echo "=== Provisioning GCP Spot Instance for G-Gate Validation ==="
    echo "Zone: ${ZONE}, Machine Type: ${MACHINE_TYPE}"

    if instance_exists; then
        echo "Instance ${INSTANCE_NAME} already exists in zone ${ZONE}."
        return 0
    fi

    echo "Creating Spot instance with 90m hard auto-delete limit (--max-run-duration=90m)..."
    gcloud compute instances create "${INSTANCE_NAME}" \
        --zone="${ZONE}" \
        --machine-type="${MACHINE_TYPE}" \
        --provisioning-model=SPOT \
        --instance-termination-action=DELETE \
        --max-run-duration=90m \
        --image-family=pytorch-latest-gpu \
        --image-project=deeplearning-platform-release \
        --maintenance-policy=TERMINATE \
        --boot-disk-size=50GB \
        --metadata=install-nvidia-driver=True

    echo "Instance ${INSTANCE_NAME} created successfully with 90-minute dead-man's switch."
}

cmd_ssh() {
    check_auth
    if ! instance_exists; then
        echo "Error: Instance ${INSTANCE_NAME} does not exist. Run '$0 up' first."
        exit 1
    fi
    gcloud compute ssh "${INSTANCE_NAME}" --zone="${ZONE}"
}

cmd_run_verify() {
    check_auth
    if ! instance_exists; then
        echo "Instance does not exist. Creating instance first..."
        cmd_up
    fi

    echo "Waiting for SSH connection to become ready..."
    for i in {1..60}; do
        if gcloud compute ssh "${INSTANCE_NAME}" --zone="${ZONE}" --command="true" 2>/dev/null; then
            break
        fi
        sleep 3
    done

    echo "Syncing repository to remote instance..."
    # Create target directory
    gcloud compute ssh "${INSTANCE_NAME}" --zone="${ZONE}" --command="mkdir -p ~/gpu-gatekeeper"
    # Copy files
    tar -czf /tmp/ggate_sync.tar.gz -C "${ROOT_DIR}" --exclude='.git' --exclude='build' --exclude='archive' .
    gcloud compute scp --zone="${ZONE}" /tmp/ggate_sync.tar.gz "${INSTANCE_NAME}:~/ggate_sync.tar.gz"
    rm -f /tmp/ggate_sync.tar.gz
    gcloud compute ssh "${INSTANCE_NAME}" --zone="${ZONE}" --command="tar -xzf ~/ggate_sync.tar.gz -C ~/gpu-gatekeeper && rm ~/ggate_sync.tar.gz"

    echo "Executing real GPU verification suite on remote GCP instance..."
    gcloud compute ssh "${INSTANCE_NAME}" --zone="${ZONE}" --command="cd ~/gpu-gatekeeper && sudo ./deploy/cloud/verify-real-gpu.sh"
}

cmd_teardown() {
    check_auth
    echo "=== Tearing Down GCP Spot Instance ==="
    if instance_exists; then
        echo "Deleting instance ${INSTANCE_NAME} in zone ${ZONE}..."
        gcloud compute instances delete "${INSTANCE_NAME}" --zone="${ZONE}" --quiet
        echo "Instance deleted successfully."
    else
        echo "Instance ${INSTANCE_NAME} does not exist."
    fi
}

SUBCMD="${1:-up}"
case "${SUBCMD}" in
    up)
        cmd_up
        ;;
    ssh)
        cmd_ssh
        ;;
    run-verify)
        cmd_run_verify
        ;;
    teardown)
        cmd_teardown
        ;;
    *)
        echo "Usage: $0 {up|ssh|run-verify|teardown}"
        exit 1
        ;;
esac
