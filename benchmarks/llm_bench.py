#!/usr/bin/env python3
"""
Hugging Face LLM Real-Hardware Benchmark for G-Gate QoS Validation & Latency Profiling.
Measures TTFT (Time To First Token), Inter-Token Latency (ITL), and Generated Tokens/sec
using autoregressive greedy decoding with KV cache on NVIDIA GPUs.
"""

import argparse
import json
import os
import sys
import time
from typing import List, Tuple

try:
    import torch
except ImportError:
    sys.exit("Error: PyTorch (torch) is required. Please install it (e.g. pip install torch).")

try:
    from transformers import AutoModelForCausalLM, AutoTokenizer
except ImportError:
    sys.exit("Error: transformers is required. Please install it (e.g. pip install transformers accelerate).")


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


def percentile(data: List[float], p: float) -> float:
    """Computes the p-th percentile of a list of floats (0-100)."""
    if not data:
        return 0.0
    sorted_data = sorted(data)
    k = (len(sorted_data) - 1) * (p / 100.0)
    f = int(k)
    c = min(f + 1, len(sorted_data) - 1)
    d = k - f
    return sorted_data[f] + d * (sorted_data[c] - sorted_data[f])


def load_model(primary_model: str, fallback_model: str):
    """Loads tokenizer and causal LM in float16 on CUDA with fallback."""
    for model_name in [primary_model, fallback_model]:
        try:
            print(f"Loading model '{model_name}'...", flush=True)
            tokenizer = AutoTokenizer.from_pretrained(model_name, trust_remote_code=True)
            if tokenizer.pad_token is None:
                tokenizer.pad_token = tokenizer.eos_token
            model = AutoModelForCausalLM.from_pretrained(
                model_name,
                dtype=torch.float16,
                device_map="cuda",
                trust_remote_code=True,
            )
            model.eval()
            print(f"Successfully loaded model '{model_name}' on {torch.cuda.get_device_name(0)}.", flush=True)
            return model, tokenizer, model_name
        except Exception as e:
            sys.stderr.write(f"Failed to load '{model_name}': {e}\n")
            if model_name == fallback_model:
                raise
    raise RuntimeError("Failed to load both primary and fallback models.")


def build_fixed_prompt(tokenizer, target_tokens: int = 128) -> torch.Tensor:
    """Constructs a deterministic prompt tensor of exact target token length."""
    base_text = (
        "You are an expert distributed systems engineer and systems researcher. "
        "Explain the key architectural tradeoffs between kernel-space eBPF QoS rate limiters, "
        "user-space API gateways, and dynamic resource allocation controllers when scheduling "
        "large language models and deep neural networks on multi-tenant GPU clusters. "
        "Discuss hardware memory bandwidth, tensor core saturation, and PCIe bus topology. "
    )
    tokens = tokenizer.encode(base_text, return_tensors="pt")
    # Repeat or slice to reach target_tokens
    while tokens.shape[1] < target_tokens:
        tokens = torch.cat([tokens, tokens], dim=1)
    tokens = tokens[:, :target_tokens].to("cuda")
    return tokens


def run_request(model, input_ids: torch.Tensor, max_new_tokens: int) -> Tuple[float, List[float]]:
    """
    Executes a single autoregressive generation request using manual greedy decode with KV cache.
    Returns:
        ttft_ms: Time to First Token in milliseconds (prefill phase).
        itl_list_ms: List of inter-token latencies in milliseconds for subsequent tokens.
    """
    # 1. Prefill step
    torch.cuda.synchronize()
    t_start = time.perf_counter()
    with torch.no_grad():
        out = model(input_ids, use_cache=True)
        next_token = out.logits[:, -1, :].argmax(dim=-1, keepdim=True)
        past_key_values = out.past_key_values
    torch.cuda.synchronize()
    t_first = time.perf_counter()
    ttft_ms = (t_first - t_start) * 1000.0

    # 2. Autoregressive decode steps
    itl_list_ms = []
    cur_token = next_token
    for _ in range(max_new_tokens - 1):
        torch.cuda.synchronize()
        t0 = time.perf_counter()
        with torch.no_grad():
            out = model(cur_token, past_key_values=past_key_values, use_cache=True)
            cur_token = out.logits[:, -1, :].argmax(dim=-1, keepdim=True)
            past_key_values = out.past_key_values
        torch.cuda.synchronize()
        t1 = time.perf_counter()
        itl_list_ms.append((t1 - t0) * 1000.0)

    return ttft_ms, itl_list_ms


def main():
    parser = argparse.ArgumentParser(description="HuggingFace LLM Real GPU Benchmark for G-Gate")
    parser.add_argument("--model", type=str, default="Qwen/Qwen2.5-0.5B-Instruct", help="Primary HuggingFace model")
    parser.add_argument("--fallback-model", type=str, default="TinyLlama/TinyLlama-1.1B-Chat-v1.0", help="Fallback model")
    parser.add_argument("--mode", type=str, choices=["decode", "prefill-flood"], default="decode", help="Benchmark mode: 'decode' or 'prefill-flood'")
    parser.add_argument("--batch-size", type=int, default=16, help="Batch size for prefill-flood mode (default: 16)")
    parser.add_argument("--seq-len", type=int, default=512, help="Sequence length for prefill-flood mode (default: 512)")
    parser.add_argument("--duration", type=float, default=60.0, help="Benchmark run time in seconds (default: 60.0)")
    parser.add_argument("--prompt-tokens", type=int, default=128, help="Prompt token count (default: 128)")
    parser.add_argument("--new-tokens", type=int, default=128, help="Generated tokens per request (default: 128)")
    parser.add_argument("--warmup-requests", type=int, default=2, help="Warmup requests excluded from metrics (default: 2)")
    parser.add_argument("--label", type=str, default="tenant", help="Output prefix label (default: 'tenant')")
    parser.add_argument("--json-out", type=str, default=None, help="Output filepath to save JSON benchmark metrics")
    args = parser.parse_args()

    if not torch.cuda.is_available():
        sys.exit("Error: CUDA is not available. An NVIDIA GPU with CUDA driver is required.")

    pid = os.getpid()
    cgroup_id = get_cgroup_id()
    gpu_name = torch.cuda.get_device_name(0)

    print(f"[{args.label}] started: pid={pid} cgroup_id={cgroup_id} gpu={gpu_name} mode={args.mode}", flush=True)

    model, tokenizer, active_model_name = load_model(args.model, args.fallback_model)

    if args.mode == "prefill-flood":
        # Prefill Flood: batch_size x seq_len forward passes with use_cache=False
        input_ids = torch.randint(low=10, high=1000, size=(args.batch_size, args.seq_len), device="cuda", dtype=torch.long)
        print(f"[{args.label}] Warming up ({args.warmup_requests} prefill batches of size {args.batch_size}x{args.seq_len})...", flush=True)
        for _ in range(args.warmup_requests):
            with torch.no_grad():
                model(input_ids, use_cache=False)
            torch.cuda.synchronize()
        print(f"[{args.label}] Warmup complete. Starting timed prefill-flood benchmark for {args.duration}s...", flush=True)

        batch_latencies_ms: List[float] = []
        batches_completed = 0
        bench_start = time.monotonic()
        while True:
            torch.cuda.synchronize()
            t0 = time.perf_counter()
            with torch.no_grad():
                model(input_ids, use_cache=False)
            torch.cuda.synchronize()
            t1 = time.perf_counter()
            batch_latencies_ms.append((t1 - t0) * 1000.0)
            batches_completed += 1

            elapsed = time.monotonic() - bench_start
            if elapsed >= args.duration:
                break

        total_elapsed_sec = time.monotonic() - bench_start
        total_prefill_tokens = batches_completed * args.batch_size * args.seq_len
        prefill_tokens_per_sec = total_prefill_tokens / total_elapsed_sec if total_elapsed_sec > 0 else 0.0

        lat_p50 = percentile(batch_latencies_ms, 50.0)
        lat_p95 = percentile(batch_latencies_ms, 95.0)
        lat_p99 = percentile(batch_latencies_ms, 99.0)
        mean_lat = sum(batch_latencies_ms) / len(batch_latencies_ms) if batch_latencies_ms else 0.0

        print(
            f"[{args.label}] finished: mode=prefill-flood batches={batches_completed} "
            f"total_tokens={total_prefill_tokens} elapsed_sec={total_elapsed_sec:.3f} "
            f"prefill_tokens_per_sec={prefill_tokens_per_sec:.2f} "
            f"batch_lat(p50={lat_p50:.2f}ms, p95={lat_p95:.2f}ms, p99={lat_p99:.2f}ms)",
            flush=True,
        )

        metrics = {
            "label": args.label,
            "pid": pid,
            "cgroup_id": cgroup_id,
            "gpu_name": gpu_name,
            "model": active_model_name,
            "mode": "prefill-flood",
            "batch_size": args.batch_size,
            "seq_len": args.seq_len,
            "duration_configured": args.duration,
            "duration_actual_sec": total_elapsed_sec,
            "batches_completed": batches_completed,
            "total_prefill_tokens": total_prefill_tokens,
            "prefill_tokens_per_sec": prefill_tokens_per_sec,
            "tokens_per_sec": prefill_tokens_per_sec,
            "batch_latency_ms": {
                "p50": lat_p50,
                "p95": lat_p95,
                "p99": lat_p99,
                "mean": mean_lat,
                "min": min(batch_latencies_ms) if batch_latencies_ms else 0.0,
                "max": max(batch_latencies_ms) if batch_latencies_ms else 0.0,
                "count": len(batch_latencies_ms),
            },
        }

    else:
        # Interactive Decode: batch_size=1 greedy decoding with KV cache
        input_ids = build_fixed_prompt(tokenizer, args.prompt_tokens)

        # Warmup runs (excluded from all metrics)
        print(f"[{args.label}] Warming up ({args.warmup_requests} requests)...", flush=True)
        for _ in range(args.warmup_requests):
            run_request(model, input_ids, args.new_tokens)
        torch.cuda.synchronize()
        print(f"[{args.label}] Warmup complete. Starting timed decode benchmark for {args.duration}s...", flush=True)

        all_ttft_ms: List[float] = []
        all_itl_ms: List[float] = []
        completed_requests = 0
        total_tokens_generated = 0

        bench_start = time.monotonic()
        while True:
            ttft, itls = run_request(model, input_ids, args.new_tokens)
            all_ttft_ms.append(ttft)
            all_itl_ms.extend(itls)
            completed_requests += 1
            total_tokens_generated += (1 + len(itls))

            elapsed = time.monotonic() - bench_start
            if elapsed >= args.duration:
                break

        total_elapsed_sec = time.monotonic() - bench_start
        tokens_per_sec = total_tokens_generated / total_elapsed_sec if total_elapsed_sec > 0 else 0.0

        ttft_p50 = percentile(all_ttft_ms, 50.0)
        ttft_p95 = percentile(all_ttft_ms, 95.0)
        itl_p50 = percentile(all_itl_ms, 50.0)
        itl_p95 = percentile(all_itl_ms, 95.0)
        itl_p99 = percentile(all_itl_ms, 99.0)

        mean_ttft = sum(all_ttft_ms) / len(all_ttft_ms) if all_ttft_ms else 0.0
        mean_itl = sum(all_itl_ms) / len(all_itl_ms) if all_itl_ms else 0.0

        print(
            f"[{args.label}] finished: mode=decode requests={completed_requests} tokens={total_tokens_generated} "
            f"elapsed_sec={total_elapsed_sec:.3f} tokens_per_sec={tokens_per_sec:.2f} "
            f"TTFT(p50={ttft_p50:.2f}ms, p95={ttft_p95:.2f}ms) "
            f"ITL(p50={itl_p50:.2f}ms, p95={itl_p95:.2f}ms, p99={itl_p99:.2f}ms)",
            flush=True,
        )

        metrics = {
            "label": args.label,
            "pid": pid,
            "cgroup_id": cgroup_id,
            "gpu_name": gpu_name,
            "model": active_model_name,
            "mode": "decode",
            "duration_configured": args.duration,
            "duration_actual_sec": total_elapsed_sec,
            "requests_completed": completed_requests,
            "total_generated_tokens": total_tokens_generated,
            "tokens_per_sec": tokens_per_sec,
            "ttft_ms": {
                "p50": ttft_p50,
                "p95": ttft_p95,
                "mean": mean_ttft,
                "min": min(all_ttft_ms) if all_ttft_ms else 0.0,
                "max": max(all_ttft_ms) if all_ttft_ms else 0.0,
                "count": len(all_ttft_ms),
            },
            "itl_ms": {
                "p50": itl_p50,
                "p95": itl_p95,
                "p99": itl_p99,
                "mean": mean_itl,
                "min": min(all_itl_ms) if all_itl_ms else 0.0,
                "max": max(all_itl_ms) if all_itl_ms else 0.0,
                "count": len(all_itl_ms),
            },
        }

    if args.json_out:
        os.makedirs(os.path.dirname(os.path.abspath(args.json_out)), exist_ok=True)
        with open(args.json_out, "w") as f:
            json.dump(metrics, f, indent=2)
        print(f"Saved metrics to {args.json_out}", flush=True)


if __name__ == "__main__":
    main()
