# Experiment 5 (LLM Decode): Predictions Written Before Results

Written 2026-11-07 09:05 EST, before any Experiment 5 data was collected. These predictions get committed alongside the results so readers can compare the two. They will not be edited afterwards.

**Setup:** Qwen2.5-0.5B-Instruct, fp16, HF transformers, greedy decoding of 128 new tokens per request, on a single Tesla T4. Scenarios: **A** one tenant alone, **B** two tenants with no quotas, **C** two tenants with G-Gate quotas at a 1:4 ratio.

## P1. A and B: interference may be weak
A 0.5B model decoding one token at a time is mostly limited by CPU work and by how fast kernels can be launched, not by the GPU's compute. We expect per-tenant tokens/sec in B to fall by **less than 2x** compared with A, possibly much less.
*If true:* this workload has little noisy-neighbor effect for G-Gate to fix, and the latency isolation story is weak *for this workload*. We will say so.

## P2. C: the 20% tenant should hit its budget
Every decode step launches roughly the same set of kernels with the same grid sizes, so G-Gate tokens spent per generated token should be close to constant. A throttled tenant's tokens/sec should therefore scale with its quota.
*Prediction:* the 20% tenant runs at **budget ÷ (G-Gate tokens per generated token) within ±10%**.
*Falsified if:* it misses by more than 10%. That would point to the cost estimate or the pause mechanism, not the token bucket.

## P3. C: the 80/20 ratio may not be 0.25
If the 80% tenant is held back by its own CPU speed rather than by its quota, the measured ratio is just (20% tenant's budgeted rate) ÷ (80% tenant's natural rate), and could land anywhere. **A ratio other than 0.25 is not a failure by itself**. The question is whether each *throttled* tenant hits its budget.

## P4. C: the throttled tenant's tail latency gets worse
G-Gate throttles by pausing the whole process (SIGSTOP) for up to 50 ms at a time. Expect the 20% tenant's **ITL p99 to rise to roughly the 10–50 ms range**, much worse than A. This is how G-Gate is designed to work, and it's a real cost for interactive low-priority tenants.

## P5. C: the high-priority tenant's latency
The 80% tenant's ITL p50 should sit **between A and B, closer to A**. If P1 holds (little interference to begin with), the difference may be within noise.
