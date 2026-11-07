# Experiment 5B (Batch Prefill Flood vs. Interactive Decode): Pre-Registered Predictions

Written 2026-11-07 10:01 EST, prior to running Experiment 5B on the AWS `g4dn.xlarge` Tesla T4.

## Workload Setup
- **High-Priority Tenant (`tenant-80` — Interactive Chat Decode):** `Qwen/Qwen2.5-0.5B-Instruct` (fp16), `batch_size=1`, greedy autoregressive decoding (`prompt_len=128`, `max_new_tokens=128`), measuring **Tokens/sec**, **TTFT (p50/p95)**, and **ITL (p50/p95/p99)**.
- **Noisy Neighbor (`tenant-20` — Batch Prefill / RAG Indexing Flood):** `Qwen/Qwen2.5-0.5B-Instruct` (fp16), continuous batched prefill forward passes (`batch_size=16`, `seq_len=512` = `8,192` prompt tokens per forward pass) in a tight loop, measuring **Prefill Tokens/sec** (`batches/sec * 8192`).

## Scenarios ($N=3 \times 45\text{s}$ trials per scenario)
1. **Scenario A (Isolated Interactive Decode):** `tenant-80` runs alone (reuse/confirm Experiment 5A baseline: `~29.9 tok/s`, `ITL p50 ~33.3 ms`, `ITL p99 ~34.8 ms`).
2. **Scenario B (Unmanaged Contention — No G-Gate):** `tenant-80` (Interactive Decode) + `tenant-20` (Batch Prefill Flood `batch=16, seq=512`) run concurrently with **no G-Gate quotas**.
3. **Scenario C (G-Gate Managed QoS):** `tenant-80` runs with an unconstrained high-priority quota (`2,000,000` G-Gate tok/s) while `tenant-20` (Batch Prefill Flood) is constrained by G-Gate's token bucket (`100,000`–`200,000` G-Gate tok/s, calibrated so `tenant-20` runs at ~20% duty cycle).

## Pre-Registered Predictions
- **P1 (Severe Unmanaged Degradation in Scenario B):** Unlike Experiment 5A (`batch=1` vs `batch=1`, which saw only `2.8%` degradation), `tenant-20`'s `batch=16, seq=512` (`8,192` tokens/pass) prefill flood will saturate the Tesla T4's Tensor Cores and SMs, degrading `tenant-80`'s interactive decode throughput by **`>40%` (to `<18 tok/s`)** and spiking `tenant-80`'s **ITL p50 / p99 into the `>60–150+ ms` range**.
- **P2 (Grid-Weighted Cost Proxy Naturally Penalizes Heavy Prefill Kernels):** Because G-Gate computes launch cost as `(gridDim * blockDim) >> 10`, a `batch=16, seq=512` prefill kernel has a much larger grid than a `batch=1, seq=1` decode kernel, so `tenant-20` consumes far more G-Gate tokens per kernel launch than `tenant-80`.
- **P3 (Interactive SLA Rescue in Scenario C):** When G-Gate throttles `tenant-20`'s prefill flood in Scenario C, `tenant-80`'s interactive decode throughput will recover substantially over Scenario B (toward its `~24.5 tok/s` uprobe-attached ceiling) and its **ITL p50 / p99 and TTFT p95 will drop sharply compared to Scenario B**.
