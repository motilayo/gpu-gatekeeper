#!/usr/bin/env bash
set -euo pipefail

# AWS Spot Smoke-Test for G-Gate Real GPU Validation
# Defaults: g4dn.xlarge Spot (~$0.15/hr) in us-east-2 with 90-minute dead-man's switch

INSTANCE_TYPE="${INSTANCE_TYPE:-g4dn.xlarge}"
REGION="${AWS_REGION:-${REGION:-us-east-2}}"
TAG_NAME="ggate-spot-smoke"
KEY_NAME="${KEY_NAME:-ggate-spot-smoke-key}"
SSH_KEY_FILE="${SSH_KEY_FILE:-/tmp/ggate-spot-smoke.pem}"
SG_NAME="ggate-spot-smoke-sg"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"

check_auth() {
    if ! command -v aws >/dev/null 2>&1; then
        echo "Error: AWS CLI is not installed or not in PATH."
        exit 1
    fi
    if ! aws sts get-caller-identity >/dev/null 2>&1; then
        echo "Error: AWS CLI credentials are missing or expired. Please refresh your AWS credentials (e.g. 'aws sso login' or export AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY) and retry."
        exit 1
    fi
}

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
    check_auth
    echo "=== Provisioning AWS Spot Instance for G-Gate Validation ==="
    echo "Region: ${REGION}, Instance Type: ${INSTANCE_TYPE}"

    EXISTING_ID=$(get_instance_id)
    if [ -n "${EXISTING_ID}" ] && [ "${EXISTING_ID}" != "None" ]; then
        echo "Found existing instance: ${EXISTING_ID}"
        IP=$(get_public_ip)
        echo "Public IP: ${IP}"
        return 0
    fi

    # 1. Resolve AMI
    echo "Resolving latest Deep Learning GPU PyTorch AMI (Ubuntu 22.04)..."
    AMI_ID=$(aws ec2 describe-images \
        --region "${REGION}" \
        --owners amazon \
        --filters "Name=name,Values=Deep Learning OSS Nvidia Driver AMI GPU PyTorch* (Ubuntu 22.04)*" "Name=state,Values=available" \
        --query "reverse(sort_by(Images, &CreationDate))[0].ImageId" \
        --output text 2>/dev/null || true)

    if [ -z "${AMI_ID}" ] || [ "${AMI_ID}" == "None" ]; then
        echo "Searching fallback Deep Learning Base GPU AMI..."
        AMI_ID=$(aws ec2 describe-images \
            --region "${REGION}" \
            --owners amazon \
            --filters "Name=name,Values=Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 22.04)*" "Name=state,Values=available" \
            --query "reverse(sort_by(Images, &CreationDate))[0].ImageId" \
            --output text 2>/dev/null || true)
    fi

    if [ -z "${AMI_ID}" ] || [ "${AMI_ID}" == "None" ]; then
        echo "Searching fallback Ubuntu 22.04 AMI..."
        AMI_ID=$(aws ec2 describe-images \
            --region "${REGION}" \
            --owners 099720109477 \
            --filters "Name=name,Values=ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-server-*" "Name=state,Values=available" \
            --query "reverse(sort_by(Images, &CreationDate))[0].ImageId" \
            --output text 2>/dev/null || true)
    fi
    echo "Selected AMI: ${AMI_ID}"

    # 2. KeyPair setup
    if [ ! -f "${SSH_KEY_FILE}" ]; then
        echo "Creating EC2 KeyPair ${KEY_NAME} -> ${SSH_KEY_FILE}..."
        aws ec2 delete-key-pair --region "${REGION}" --key-name "${KEY_NAME}" 2>/dev/null || true
        aws ec2 create-key-pair \
            --region "${REGION}" \
            --key-name "${KEY_NAME}" \
            --query 'KeyMaterial' \
            --output text > "${SSH_KEY_FILE}"
        chmod 600 "${SSH_KEY_FILE}"
    else
        echo "Using existing SSH key file: ${SSH_KEY_FILE}"
    fi

    # 3. Security Group setup in default VPC
    SG_ID=$(aws ec2 describe-security-groups \
        --region "${REGION}" \
        --filters "Name=group-name,Values=${SG_NAME}" \
        --query "SecurityGroups[0].GroupId" \
        --output text 2>/dev/null || true)

    if [ -z "${SG_ID}" ] || [ "${SG_ID}" == "None" ]; then
        echo "Creating Security Group ${SG_NAME} in default VPC..."
        VPC_ID=$(aws ec2 describe-vpcs \
            --region "${REGION}" \
            --filters "Name=isDefault,Values=true" \
            --query "Vpcs[0].VpcId" \
            --output text 2>/dev/null || true)
        VPC_ARG=()
        if [ -n "${VPC_ID}" ] && [ "${VPC_ID}" != "None" ]; then
            VPC_ARG=(--vpc-id "${VPC_ID}")
        fi
        SG_ID=$(aws ec2 create-security-group \
            --region "${REGION}" \
            --group-name "${SG_NAME}" \
            --description "Security group for G-Gate Spot GPU smoke test" \
            ${VPC_ARG[@]+"${VPC_ARG[@]}"} \
            --query 'GroupId' \
            --output text)
    fi
    echo "Using Security Group: ${SG_NAME} (${SG_ID})"

    MY_IP=$(curl -s https://checkip.amazonaws.com 2>/dev/null || true)
    if [ -n "${MY_IP}" ]; then
        echo "Authorizing SSH ingress from ${MY_IP}/32..."
        aws ec2 authorize-security-group-ingress \
            --region "${REGION}" \
            --group-id "${SG_ID}" \
            --protocol tcp \
            --port 22 \
            --cidr "${MY_IP}/32" 2>/dev/null || true
    fi

    # 4. 90-minute dead-man's switch
    USER_DATA_B64=$(echo -e '#!/bin/bash\nshutdown -h +90 "G-Gate 90-minute safety auto-terminate"' | base64)

    launch_instance() {
        local itype="$1"
        local is_spot="$2"
        local spot_arg=()
        if [ "${is_spot}" = "true" ]; then
            spot_arg=(--instance-market-options '{"MarketType":"spot","SpotOptions":{"SpotInstanceType":"one-time"}}')
        fi

        aws ec2 run-instances \
            --region "${REGION}" \
            --image-id "${AMI_ID}" \
            --instance-type "${itype}" \
            ${spot_arg[@]+"${spot_arg[@]}"} \
            --key-name "${KEY_NAME}" \
            --security-group-ids "${SG_ID}" \
            --instance-initiated-shutdown-behavior terminate \
            --user-data "${USER_DATA_B64}" \
            --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=${TAG_NAME}}]" \
            --query "Instances[0].InstanceId" \
            --output text
    }

    INSTANCE_ID=""
    echo "Attempting to launch ${INSTANCE_TYPE} Spot instance..."
    set +e
    RUN_OUT=$(launch_instance "${INSTANCE_TYPE}" "true" 2>&1)
    RUN_STATUS=$?
    set -e

    if [ ${RUN_STATUS} -eq 0 ] && [ -n "${RUN_OUT}" ] && [ "${RUN_OUT}" != "None" ]; then
        INSTANCE_ID="${RUN_OUT}"
    else
        echo "Spot launch for ${INSTANCE_TYPE} failed: ${RUN_OUT}"
        if echo "${RUN_OUT}" | grep -qE "InsufficientInstanceCapacity|SpotMaxPriceTooLow|MaxSpotInstanceCountExceeded"; then
            echo "Falling back to g5.xlarge Spot..."
            set +e
            RUN_OUT=$(launch_instance "g5.xlarge" "true" 2>&1)
            RUN_STATUS=$?
            set -e
            if [ ${RUN_STATUS} -eq 0 ] && [ -n "${RUN_OUT}" ] && [ "${RUN_OUT}" != "None" ]; then
                INSTANCE_ID="${RUN_OUT}"
                INSTANCE_TYPE="g5.xlarge"
            else
                echo "g5.xlarge Spot failed: ${RUN_OUT}"
                echo "Falling back to On-Demand ${INSTANCE_TYPE} with 90m auto-terminate safety switch..."
                set +e
                RUN_OUT=$(launch_instance "${INSTANCE_TYPE}" "false" 2>&1)
                RUN_STATUS=$?
                set -e
                if [ ${RUN_STATUS} -eq 0 ] && [ -n "${RUN_OUT}" ] && [ "${RUN_OUT}" != "None" ]; then
                    INSTANCE_ID="${RUN_OUT}"
                else
                    echo "On-Demand launch failed: ${RUN_OUT}"
                    exit 1
                fi
            fi
        else
            echo "Error running instance: ${RUN_OUT}"
            exit 1
        fi
    fi

    echo "Instance launched: ${INSTANCE_ID}"
    echo "Waiting for instance to enter running state..."
    aws ec2 wait instance-running --region "${REGION}" --instance-ids "${INSTANCE_ID}"

    IP=$(get_public_ip)
    echo "Instance is running! Public IP: ${IP}"
    echo "Dead-man's switch active: instance will automatically terminate in 90 minutes if not torn down."
}

cmd_ssh() {
    check_auth
    IP=$(get_public_ip)
    if [ -z "${IP}" ] || [ "${IP}" == "None" ]; then
        echo "Error: No running instance found."
        exit 1
    fi
    SSH_OPTS=(-o "StrictHostKeyChecking=no")
    if [ -n "${SSH_KEY_FILE}" ] && [ -f "${SSH_KEY_FILE}" ]; then
        SSH_OPTS+=(-i "${SSH_KEY_FILE}")
    fi
    ssh "${SSH_OPTS[@]}" "ubuntu@${IP}"
}

cmd_run_verify() {
    check_auth
    IP=$(get_public_ip)
    if [ -z "${IP}" ] || [ "${IP}" == "None" ]; then
        echo "No running instance found. Starting instance first..."
        cmd_up
        IP=$(get_public_ip)
    fi

    SSH_OPTS=(-o "StrictHostKeyChecking=no")
    RSYNC_SSH="ssh -o StrictHostKeyChecking=no"
    if [ -n "${SSH_KEY_FILE}" ] && [ -f "${SSH_KEY_FILE}" ]; then
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
    check_auth
    echo "=== Tearing Down AWS Spot Instance ==="
    INSTANCE_IDS=$(aws ec2 describe-instances \
        --region "${REGION}" \
        --filters "Name=tag:Name,Values=${TAG_NAME}" "Name=instance-state-name,Values=pending,running,stopping,stopped" \
        --query "Reservations[].Instances[].InstanceId" \
        --output text 2>/dev/null || true)

    if [ -n "${INSTANCE_IDS}" ] && [ "${INSTANCE_IDS}" != "None" ]; then
        echo "Terminating instances: ${INSTANCE_IDS}"
        aws ec2 terminate-instances --region "${REGION}" --instance-ids ${INSTANCE_IDS}
        echo "Waiting for instance termination..."
        aws ec2 wait instance-terminated --region "${REGION}" --instance-ids ${INSTANCE_IDS} 2>/dev/null || true
        echo "Instances terminated."
    else
        echo "No running or pending instances found with tag Name=${TAG_NAME}."
    fi

    echo "Deleting key-pair ${KEY_NAME}..."
    aws ec2 delete-key-pair --region "${REGION}" --key-name "${KEY_NAME}" 2>/dev/null || true
    rm -f "${SSH_KEY_FILE}"

    echo "Deleting security group ${SG_NAME}..."
    aws ec2 delete-security-group --region "${REGION}" --group-name "${SG_NAME}" 2>/dev/null || true
    echo "Teardown complete."
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
