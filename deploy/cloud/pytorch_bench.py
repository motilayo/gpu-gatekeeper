#!/usr/bin/env python3
"""
PyTorch Real GPU Benchmark for G-Gate QoS Validation.
Allocates CUDA VRAM, executes matrix multiplications in a timed loop,
and prints start/finish telemetry including cgroup v2 inode and achieved IOPS.
"""

import argparse
import os
import sys
import time

try:
    import torch
except ImportError:
    sys.exit("Error: PyTorch (torch) is required. Please install it (e.g. pip install torch).")


def get_cgroup_id() -> int:
    """Resolves the current process's cgroup v2 inode."""
    try:
        with open("/proc/self/cgroup", "r") as f:
            for line in f:
                line = line.strip()
                if line.startswith("0::"):
                    rel = line[3:].lstrip("/")
                    cgroup_path = os.path.join("/sys/fs/cgroup", rel) if rel else "/sys/fs/cgroup"
                    if os.path.exists(cgroup_path):
                        return os.stat(cgroup_path).st_ino
        if os.path.exists("/sys/fs/cgroup"):
            return os.stat("/sys/fs/cgroup").st_ino
    except Exception as e:
        sys.stderr.write(f"Warning resolving cgroup inode: {e}\n")
    return 0


def main():
    parser = argparse.ArgumentParser(description="PyTorch Real GPU Benchmark for G-Gate")
    parser.add_argument("--duration", type=float, default=10.0, help="Benchmark run time in seconds (default: 10.0)")
    parser.add_argument("--matrix-size", type=int, default=256, help="Matrix dimension N for N x N matmul (default: 256)")
    parser.add_argument("--vram-mb", type=int, default=256, help="CUDA VRAM buffer size in MiB (default: 256)")
    parser.add_argument("--free-on-exit", action="store_true", help="Release allocated CUDA memory and empty cache on exit")
    parser.add_argument("--label", type=str, default="tenant", help="Output prefix label (default: 'tenant')")
    args = parser.parse_args()

    if not torch.cuda.is_available():
        sys.exit("Error: CUDA is not available on this machine. An NVIDIA GPU with CUDA driver is required.")

    pid = os.getpid()
    cgroup_id = get_cgroup_id()
    gpu_name = torch.cuda.get_device_name(0)

    print(f"[{args.label}] started: pid={pid} cgroup_id={cgroup_id} gpu={gpu_name}", flush=True)

    # Allocate target VRAM buffer on CUDA
    vram_bytes = args.vram_mb * 1024 * 1024
    num_floats = max(1, vram_bytes // 4)
    buf = torch.empty((num_floats,), dtype=torch.float32, device="cuda")

    # Allocate input matrices for compute loop
    n = args.matrix_size
    a = torch.ones((n, n), dtype=torch.float32, device="cuda")
    b = torch.ones((n, n), dtype=torch.float32, device="cuda")
    torch.cuda.synchronize()

    # Warmup iterations to initialize cuBLAS handle and load fatbins
    for _ in range(2):
        c = torch.matmul(a, b)
        torch.cuda.synchronize()

    # Timed compute loop
    start = time.monotonic()
    iters = 0
    c = None
    while True:
        c = torch.matmul(a, b)
        torch.cuda.synchronize()
        iters += 1

        now = time.monotonic()
        if now - start >= args.duration:
            break

    elapsed_sec = time.monotonic() - start
    iops = iters / elapsed_sec if elapsed_sec > 0 else 0.0

    if args.free_on_exit:
        del buf, a, b, c
        torch.cuda.empty_cache()
        torch.cuda.synchronize()

    print(f"[{args.label}] finished: iters={iters} elapsed_sec={elapsed_sec:.6f} iops={iops:.2f}", flush=True)


if __name__ == "__main__":
    main()
