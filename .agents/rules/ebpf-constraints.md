# G-Gate Project Rules & eBPF Verifier Constraints

## Tech Stack
- **Language:** Go 1.23+ (Control Plane) and C (`clang -target bpf -O2 -g` via `bpf2go` from `github.com/cilium/ebpf`).
- **Target OS:** Linux Kernel 5.15+ with BTF enabled (`vmlinux.h`) and `cgroup v2` unified hierarchy.

## Strict eBPF Verifier Rules (MUST FOLLOW)
1. **No Floating Point:** Never use `float` or `double` in `.bpf.c` files. Use integer bit-shifts or `1000000000ULL` nanosecond scaling for token bucket math.
2. **512-Byte Stack Limit:** Never allocate large structs on the eBPF stack. Use `bpf_ringbuf_reserve()` for ring buffer events or per-CPU array maps for scratch space.
3. **Null-Check Every Map Lookup:** Every call to `bpf_map_lookup_elem()` MUST be immediately followed by an explicit `if (!val) return 0;` check before dereferencing.
4. **Division by Zero Guard:** Before dividing by `state->refill_rate_tokens_per_sec` in eBPF C code, always check `if (state->refill_rate_tokens_per_sec == 0) return 0;` so the eBPF verifier proves division-by-zero is impossible.
5. **Prevent Symbol Inlining in Mocks:** Any mock C functions in `mock/libcuda_mock.c` MUST use `__attribute__((noinline, used, visibility("default")))` and include an `asm volatile("");` barrier so `gcc`/`clang` does not optimize them away.

## macOS Apple Silicon (M1 Pro) Execution & Multi-Arch Rules
1. **Host vs. Guest Execution:** The workspace is hosted on macOS (`arm64`), which does not have a Linux kernel. NEVER run `go generate`, `go build`, `clang`, or test binaries directly on the macOS host. Always prefix build, codegen, and test commands with:
   `limactl shell ggate -- <command>`
2. **Multi-Arch `bpf2go` Directive:** Because `PT_REGS_PARM*` macros require explicit CPU architecture definitions (`__TARGET_ARCH_arm64` and `__TARGET_ARCH_x86`), the `go:generate` directive MUST explicitly target both architectures:
   `//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target arm64,amd64 -cc clang -cflags "-O2 -g -Wall -Werror" ggate ./bpf/ggate.bpf.c`
3. **Header Order in `.bpf.c`:** Always include `"vmlinux.h"` first, followed by `<bpf/bpf_helpers.h>` and `<bpf/bpf_tracing.h>`.
4. **Cgroup v2 Test Isolation:** Inside the Lima VM, run simulated workloads inside isolated transient cgroup v2 slices using `sudo systemd-run --scope --slice=ggate-test.slice ./build/workload_sim` so each simulated tenant gets a distinct `cgroup_id` inode.
