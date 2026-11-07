# Tokenomics & Multi-Model Work Delegation Rules

## Core Principle
Optimize token efficiency and execution speed by separating high-level architectural reasoning from low-level implementation and shell execution:

1. **Orchestrator / High-Reasoning Tier (Argon / Pro / Claude):**
   - Owns system architecture, specification design (`spec.md`), eBPF verifier constraint modeling, implementation plans, code review, and complex debugging.
   - Acts as the coordinator: breaks work into well-specified, self-contained tasks with exact file paths, interfaces, and acceptance criteria.
   - Avoids spending high-tier tokens on verbose boilerplate generation or long-running shell/setup tasks.

2. **Execution / Code-Gen Tier (`flash` and `flash_lite` Subagents):**
   - Always pass `Model: "flash"` (or `"flash_lite"` for trivial tasks) to `invoke_subagent` when delegating:
     - Environment setup, package installation (`brew install`, `apt-get`, `limactl start`), and build execution.
     - Writing C/Go implementation files, tests, and `Makefile` targets from an approved specification or plan.
     - Running verification harnesses, scraping metrics, and collecting command outputs.
   - Prompts sent to `flash` subagents must include clear, unambiguous specifications (struct definitions, function signatures, and file paths) so the subagent can execute cleanly in minimal turns.
