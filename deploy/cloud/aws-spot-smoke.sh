#!/usr/bin/env bash
set -euo pipefail

# AWS Spot Smoke-Test for G-Gate Real GPU Validation
# Defaults: g4dn.xlarge Spot (~$0.15/hr) in us-east-1 with 90-minute dead-man's switch

INSTANCE_TYPE="${INSTANCE_TYPE:-g4dn.xlarge}"
REGION="${AWS_REGION:-${REGION:-us-east-1}}"
TAG_NAME="ggate-spot-smoke"
KEY_NAME="${KEY_NAME:-}"
SSH_KEY_FILE="${SSH_KEY_FILE:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"

get_instance_id() {
    aws ec2 describe-instances \
        --region "${REGION}" \
        --filters "Name=tag:Name,Values=${TAG_NAME}" "Name=instance-state-name,Values=pending,running" \
        --query "Reservations[0].Instances[0].InstanceId" \
        --output text
}

get_public_ip() {
    aws ec2 describe-instances \
        --region "${REGION}" \
        --filters "Name=tag:Name,Values=${TAG_NAME}" "Name=instance-state-name,Values=running" \
        --query "Reservations[0].Instances[0].PublicIpAddress" \
        --output text
}

cmd_up() {
    echo "=== Provisioning AWS Spot Instance for G-Gate Validation ==="
    echo "Region: ${REGION}, Instance Type: ${INSTANCE_TYPE}"

    EXISTING_ID=$(get_instance_id)
    if [ -n "${EXISTING_ID}" ] && [ "${EXISTING_ID}" != "None" ]; then
        echo "Found existing instance: ${EXISTING_ID}"
        IP=$(get_public_ip)
        echo "Public IP: ${IP}"
        return 0
    fi

    echo "Resolving latest Deep Learning Base GPU AMI (Ubuntu 22.04)..."
    AMI_ID=$(aws ec2 describe-images \
        --region "${REGION}" \
        --owners amazon \
        --filters "Name=name,Values=Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 22.04)*" "Name=state,Values=available" \
        --query "reverse(sort_by(Images, &CreationDate))[0].ImageId" \
        --output text)

    if [ -z "${AMI_ID}" ] || [ "${AMI_ID}" == "None" ]; then
        echo "Searching fallback Ubuntu 22.04 AMI..."
        AMI_ID=$(aws ec2 describe-images \
            --region "${REGION}" \
            --owners 099720109477 \
            --filters "Name=name,Values=ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-server-*" "Name=state,Values=available" \
            --query "reverse(sort_by(Images, &CreationDate))[0].ImageId" \
            --output text)
    fi
    echo "Selected AMI: ${AMI_ID}"

    # 90-minute dead-man's switch: instance automatically shuts down and terminates after 90m
    USER_DATA_B64=$(echo -e '#!/bin/bash\nshutdown -h +90 "G-Gate 90-minute safety auto-terminate"' | base64)

    EXTRA_ARGS=()
    if [ -n "${KEY_NAME}" ]; then
        EXTRA_ARGS+=(--key-name "${KEY_NAME}")
    fi

    echo "Launching ${INSTANCE_TYPE} Spot instance with 90-minute safety auto-terminate..."
    INSTANCE_ID=$(aws ec2 run-instances \
        --region "${REGION}" \
        --image-id "${AMI_ID}" \
        --instance-type "${INSTANCE_TYPE}" \
        --instance-market-options '{"MarketType":"spot","SpotOptions":{"SpotInstanceType":"one-time"}}' \
        --instance-initiated-shutdown-behavior terminate \
        --user-data "${USER_DATA_B64}" \
        --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=${TAG_NAME}}]" \
        "${EXTRA_ARGS[@]}" \
        --query "Instances[0].InstanceId" \
        --output text)

    echo "Spot instance requested: ${INSTANCE_ID}"
    echo "Waiting for instance to enter running state..."
    aws ec2 wait instance-running --region "${REGION}" --instance-ids "${INSTANCE_ID}"

    IP=$(get_public_ip)
    echo "Instance is running! Public IP: ${IP}"
    echo "Dead-man's switch active: instance will automatically terminate in 90 minutes if not torn down."
}

cmd_ssh() {
    IP=$(get_public_ip)
    if [ -z "${IP}" ] || [ "${IP}" == "None" ]; then
        echo "Error: No running instance found."
        exit 1
    fi
    SSH_OPTS=(-o "StrictHostKeyChecking=no")
    if [ -n "${SSH_KEY_FILE}" ]; then
        SSH_OPTS+=(-i "${SSH_KEY_FILE}")
    fi
    ssh "${SSH_OPTS[@]}" "ubuntu@${IP}"
}

cmd_run_verify() {
    IP=$(get_public_ip)
    if [ -z "${IP}" ] || [ "${IP}" == "None" ]; then
        echo "No running instance found. Starting instance first..."
        cmd_up
        IP=$(get_public_ip)
    fi

    SSH_OPTS=(-o "StrictHostKeyChecking=no")
    RSYNC_SSH="ssh -o StrictHostKeyChecking=no"
    if [ -n "${SSH_KEY_FILE}" ]; then
        SSH_OPTS+=(-i "${SSH_KEY_FILE}")
        RSYNC_SSH="ssh -o StrictHostKeyChecking=no -i ${SSH_KEY_FILE}"
    fi

    echo "Waiting for SSH to become available on ${IP}..."
    for i in {1..60}; do
        if ssh "${SSH_OPTS[@]}" -o "ConnectTimeout=5" "ubuntu@${IP}" "true" 2>/dev/null; then
            break
        fi
        sleep 3
    done

    echo "Syncing repository to remote instance..."
    rsync -avz -e "${RSYNC_SSH}" \
        --exclude '.git' \
        --exclude 'build' \
        --exclude 'archive' \
        "${ROOT_DIR}/" "ubuntu@${IP}:~/gpu-gatekeeper/"

    echo "Executing real GPU verification suite on remote instance..."
    ssh "${SSH_OPTS[@]}" -t "ubuntu@${IP}" "cd ~/gpu-gatekeeper && sudo ./deploy/cloud/verify-real-gpu.sh"
}

cmd_teardown() {
    echo "=== Tearing Down AWS Spot Instance ==="
    INSTANCE_IDS=$(aws ec2 describe-instances \
        --region "${REGION}" \
        --filters "Name=tag:Name,Values=${TAG_NAME}" "Name=instance-state-name,Values=pending,running,stopping,stopped" \
        --query "Reservations[].Instances[].InstanceId" \
        --output text)

    if [ -z "${INSTANCE_IDS}" ] || [ "${INSTANCE_IDS}" == "None" ]; then
        echo "No running or pending instances found with tag Name=${TAG_NAME}."
        return 0
    fi

    echo "Terminating instances: ${INSTANCE_IDS}"
    aws ec2 terminate-instances --region "${REGION}" --instance-ids ${INSTANCE_IDS}
    echo "Instances termination initiated."
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
