#!/usr/bin/env python3
"""
PyTorch Real GPU Benchmark for G-Gate QoS Validation & Latency Profiling.
Allocates CUDA VRAM, executes matrix multiplications or CUDA Graph replays
in a timed loop, measures per-iteration latency, and exports JSON / stdout metrics.
"""

import argparse
import json
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
    parser.add_argument("--cuda-graph", action="store_true", help="Capture and replay via torch.cuda.CUDAGraph (exercises cuGraphLaunch)")
    parser.add_argument("--json-out", type=str, default=None, help="Optional output filepath to save JSON benchmark metrics")
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
    c = None
    for _ in range(2):
        c = torch.matmul(a, b)
        torch.cuda.synchronize()

    # Capture CUDA Graph if requested
    g = None
    if args.cuda_graph:
        g = torch.cuda.CUDAGraph()
        with torch.cuda.graph(g):
            c = torch.matmul(a, b)
        torch.cuda.synchronize()

    # Timed compute loop with per-iteration latency recording
    step_latencies_ms = []
    start = time.monotonic()
    iters = 0

    while True:
        t0 = time.perf_counter()
        if g is not None:
            g.replay()
        else:
            c = torch.matmul(a, b)
        torch.cuda.synchronize()
        t1 = time.perf_counter()

        step_latencies_ms.append((t1 - t0) * 1000.0)
        iters += 1

        now = time.monotonic()
        if now - start >= args.duration:
            break

    elapsed_sec = time.monotonic() - start
    iops = iters / elapsed_sec if elapsed_sec > 0 else 0.0

    # Calculate latency percentiles
    if step_latencies_ms:
        sorted_lat = sorted(step_latencies_ms)
        n_steps = len(sorted_lat)
        mean_ms = sum(sorted_lat) / n_steps
        p50_ms = sorted_lat[int(n_steps * 0.50)]
        p95_ms = sorted_lat[min(int(n_steps * 0.95), n_steps - 1)]
        p99_ms = sorted_lat[min(int(n_steps * 0.99), n_steps - 1)]
    else:
        mean_ms = p50_ms = p95_ms = p99_ms = 0.0

    if args.free_on_exit:
        del buf, a, b, c, g
        torch.cuda.empty_cache()
        torch.cuda.synchronize()

    # Optional JSON export
    if args.json_out:
        out_data = {
            "label": args.label,
            "pid": pid,
            "cgroup_id": cgroup_id,
            "gpu": gpu_name,
            "matrix_size": args.matrix_size,
            "vram_mb": args.vram_mb,
            "cuda_graph": args.cuda_graph,
            "free_on_exit": args.free_on_exit,
            "iters": iters,
            "elapsed_sec": round(elapsed_sec, 6),
            "iops": round(iops, 2),
            "latency_mean_ms": round(mean_ms, 4),
            "latency_p50_ms": round(p50_ms, 4),
            "latency_p95_ms": round(p95_ms, 4),
            "latency_p99_ms": round(p99_ms, 4),
        }
        out_dir = os.path.dirname(os.path.abspath(args.json_out))
        if out_dir:
            os.makedirs(out_dir, exist_ok=True)
        with open(args.json_out, "w") as f:
            json.dump(out_data, f, indent=2)

    print(
        f"[{args.label}] finished: iters={iters} elapsed_sec={elapsed_sec:.6f} iops={iops:.2f} "
        f"p50_ms={p50_ms:.3f} p95_ms={p95_ms:.3f} p99_ms={p99_ms:.3f}",
        flush=True,
    )


if __name__ == "__main__":
    main()
