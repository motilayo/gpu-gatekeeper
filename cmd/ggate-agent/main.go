package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/motilayo/gpu-gatekeeper/internal/bpf"
	"github.com/motilayo/gpu-gatekeeper/internal/cgroup"
	"github.com/motilayo/gpu-gatekeeper/internal/dra"
	"github.com/motilayo/gpu-gatekeeper/internal/k8s"
)

type quotaList []string

func (q *quotaList) String() string {
	return strings.Join(*q, ",")
}

func (q *quotaList) Set(val string) error {
	*q = append(*q, val)
	return nil
}

type wallPauseTracker struct {
	mu         sync.RWMutex
	nsByCgroup map[uint64]uint64
}

func newWallPauseTracker() *wallPauseTracker {
	return &wallPauseTracker{
		nsByCgroup: make(map[uint64]uint64),
	}
}

func (t *wallPauseTracker) Add(cgroupID, ns uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nsByCgroup[cgroupID] += ns
}

func (t *wallPauseTracker) Get(cgroupID uint64) uint64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.nsByCgroup[cgroupID]
}

type bpfCollector struct {
	quotaMap            *ebpf.Map
	resolver            *k8s.Resolver
	pauseTracker        *wallPauseTracker
	launchesDesc        *prometheus.Desc
	throttledDesc       *prometheus.Desc
	throttledSecDesc    *prometheus.Desc
	vramDesc            *prometheus.Desc
	vramQuotaDesc       *prometheus.Desc
	vramQuotaExceedDesc *prometheus.Desc
}

func newBPFCollector(quotaMap *ebpf.Map, resolver *k8s.Resolver, pauseTracker *wallPauseTracker) *bpfCollector {
	labels := []string{"cgroup_id", "namespace", "pod", "container", "resource_claim"}
	return &bpfCollector{
		quotaMap:     quotaMap,
		resolver:     resolver,
		pauseTracker: pauseTracker,
		launchesDesc: prometheus.NewDesc(
			"ggate_gpu_launches_total",
			"Total number of GPU kernel launches intercepted",
			labels,
			nil,
		),
		throttledDesc: prometheus.NewDesc(
			"ggate_gpu_throttled_total",
			"Total number of throttled GPU kernel launches",
			labels,
			nil,
		),
		throttledSecDesc: prometheus.NewDesc(
			"ggate_gpu_throttled_seconds_total",
			"Total time processes were paused by the QoS throttling governor",
			labels,
			nil,
		),
		vramDesc: prometheus.NewDesc(
			"ggate_vram_allocated_bytes",
			"Total allocated VRAM in bytes",
			labels,
			nil,
		),
		vramQuotaDesc: prometheus.NewDesc(
			"ggate_vram_quota_bytes",
			"Configured maximum VRAM quota in bytes",
			labels,
			nil,
		),
		vramQuotaExceedDesc: prometheus.NewDesc(
			"ggate_vram_quota_exceeded_total",
			"Total number of memory allocations that exceeded the configured VRAM quota",
			labels,
			nil,
		),
	}
}

func (c *bpfCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.launchesDesc
	ch <- c.throttledDesc
	ch <- c.throttledSecDesc
	ch <- c.vramDesc
	ch <- c.vramQuotaDesc
	ch <- c.vramQuotaExceedDesc
}

func (c *bpfCollector) Collect(ch chan<- prometheus.Metric) {
	var cgroupID uint64
	var state bpf.CgroupQuotaState

	iter := c.quotaMap.Iterate()
	for iter.Next(&cgroupID, &state) {
		cgStr := strconv.FormatUint(cgroupID, 10)
		meta := c.resolver.LookupMeta(cgroupID)

		throttledNs := c.pauseTracker.Get(cgroupID)
		if throttledNs < state.TotalThrottledNs {
			throttledNs = state.TotalThrottledNs
		}

		ch <- prometheus.MustNewConstMetric(
			c.launchesDesc,
			prometheus.CounterValue,
			float64(state.TotalLaunches),
			cgStr, meta.Namespace, meta.Pod, meta.Container, meta.ResourceClaim,
		)
		ch <- prometheus.MustNewConstMetric(
			c.throttledDesc,
			prometheus.CounterValue,
			float64(state.TotalThrottled),
			cgStr, meta.Namespace, meta.Pod, meta.Container, meta.ResourceClaim,
		)
		ch <- prometheus.MustNewConstMetric(
			c.throttledSecDesc,
			prometheus.CounterValue,
			float64(throttledNs)/1e9,
			cgStr, meta.Namespace, meta.Pod, meta.Container, meta.ResourceClaim,
		)
		ch <- prometheus.MustNewConstMetric(
			c.vramDesc,
			prometheus.GaugeValue,
			float64(state.AllocatedVramBytes),
			cgStr, meta.Namespace, meta.Pod, meta.Container, meta.ResourceClaim,
		)
		ch <- prometheus.MustNewConstMetric(
			c.vramQuotaDesc,
			prometheus.GaugeValue,
			float64(state.MaxVramBytes),
			cgStr, meta.Namespace, meta.Pod, meta.Container, meta.ResourceClaim,
		)
		ch <- prometheus.MustNewConstMetric(
			c.vramQuotaExceedDesc,
			prometheus.CounterValue,
			float64(state.VramQuotaExceeded),
			cgStr, meta.Namespace, meta.Pod, meta.Container, meta.ResourceClaim,
		)
	}
	if err := iter.Err(); err != nil {
		log.Printf("error iterating cgroup_quota_map: %v", err)
	}
}

func resolveLibcudaPath(configured string) (string, error) {
	if configured != "" && configured != "auto" {
		if _, err := os.Stat(configured); err == nil {
			return configured, nil
		}
	}

	candidates := []string{
		"/run/nvidia/driver/usr/lib/x86_64-linux-gnu/libcuda.so.1",
		"/run/nvidia/driver/usr/lib64/libcuda.so.1",
		"/usr/local/nvidia/lib64/libcuda.so.1",
		"/usr/lib/x86_64-linux-gnu/libcuda.so.1",
		"/lib/x86_64-linux-gnu/libcuda.so.1",
		"/usr/lib/aarch64-linux-gnu/libcuda.so.1",
		"/lib/aarch64-linux-gnu/libcuda.so.1",
		"/usr/lib64/libcuda.so.1",
		"/proc/1/root/run/nvidia/driver/usr/lib/x86_64-linux-gnu/libcuda.so.1",
		"/proc/1/root/run/nvidia/driver/usr/lib64/libcuda.so.1",
		"/proc/1/root/usr/local/nvidia/lib64/libcuda.so.1",
		"/proc/1/root/usr/lib/x86_64-linux-gnu/libcuda.so.1",
		"/proc/1/root/lib/x86_64-linux-gnu/libcuda.so.1",
		"/proc/1/root/usr/lib/aarch64-linux-gnu/libcuda.so.1",
		"/proc/1/root/lib/aarch64-linux-gnu/libcuda.so.1",
		"/proc/1/root/usr/lib64/libcuda.so.1",
		"./build/libcuda.so",
	}

	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("no libcuda.so found in candidate search paths (configured: %q)", configured)
}

func main() {
	var quotas quotaList
	flag.Var(&quotas, "quota", "Cgroup quota in format target:refill_rate:max_burst[:max_vram_bytes] (can be specified multiple times, target can be numeric inode or path/glob)")
	vramPolicyStr := flag.String("vram-policy", "audit", "VRAM quota enforcement policy: audit, throttle, or kill")
	maxVramMb := flag.Uint64("max-vram-mb", 0, "Default maximum VRAM quota in MiB for CLI-configured quotas")
	libcudaPath := flag.String("libcuda", "./build/libcuda.so", "Path to libcuda.so mock library or 'auto' for driver discovery")
	metricsAddr := flag.String("metrics-addr", ":9090", "Prometheus metrics HTTP address")
	kubeletPodsURL := flag.String("kubelet-pods-url", "", "HTTP URL returning Kubelet /pods JSON")
	kubeletPodsFile := flag.String("kubelet-pods-file", "", "Path to local JSON file with Kubelet /pods format")
	kubeletTokenFile := flag.String("kubelet-token-file", "/var/run/secrets/kubernetes.io/serviceaccount/token", "Path to ServiceAccount bearer token for Kubelet HTTPS :10250/pods")
	draClaimsURL := flag.String("dra-claims-url", "", "HTTPS URL returning Kubernetes resource.k8s.io/v1 ResourceClaimList JSON")
	draClaimsFile := flag.String("dra-claims-file", "", "Path to local JSON file with Kubernetes ResourceClaimList format")
	cdiDir := flag.String("cdi-dir", "/var/run/cdi", "Path to directory for Container Device Interface (CDI) JSON specs")
	draSock := flag.String("dra-sock", "", "Path to UNIX domain socket for DRA NodePrepareResources / NodeUnprepareResources RPC server")
	nodeName := flag.String("node-name", os.Getenv("NODE_NAME"), "Node name for Kubernetes DRA ResourceSlice publishing")
	draSliceURL := flag.String("dra-slice-url", "", "HTTPS URL for Kubernetes API server to publish node ResourceSlice")
	draSliceFile := flag.String("dra-slice-file", "", "Path to local file where node ResourceSlice JSON should be written")
	flag.Parse()

	defaultVramPolicy := k8s.ParseVRAMPolicy(*vramPolicyStr)

	if err := rlimit.RemoveMemlock(); err != nil {
		log.Printf("warning: failed to remove memlock limit: %v", err)
	}

	var objs bpf.GgateObjects
	if err := bpf.LoadGgateObjects(&objs, nil); err != nil {
		log.Fatalf("loading eBPF objects: %v", err)
	}
	defer objs.Close()

	var watcherSpecs []cgroup.QuotaSpec

	// Configure manual CLI quotas
	for _, q := range quotas {
		parts := strings.Split(q, ":")
		if len(parts) != 3 && len(parts) != 4 {
			log.Fatalf("invalid --quota format %q, expected target:refill_rate:max_burst[:max_vram_bytes]", q)
		}
		target := parts[0]
		refill, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			log.Fatalf("invalid refill_rate in quota %q: %v", q, err)
		}
		maxBurst, err := strconv.ParseUint(parts[2], 10, 64)
		if err != nil {
			log.Fatalf("invalid max_burst in quota %q: %v", q, err)
		}
		var maxVram uint64
		if len(parts) == 4 {
			maxVram, err = strconv.ParseUint(parts[3], 10, 64)
			if err != nil {
				log.Fatalf("invalid max_vram_bytes in quota %q: %v", q, err)
			}
		}
		if maxVram == 0 && *maxVramMb > 0 {
			maxVram = *maxVramMb * 1024 * 1024
		}

		// Check if target is a numeric cgroup_id inode
		cgID, err := strconv.ParseUint(target, 10, 64)
		if err == nil {
			state := bpf.CgroupQuotaState{
				RefillRateTokensPerSec: refill,
				MaxBurstTokens:         maxBurst,
				CurrentTokens:          maxBurst,
				MaxVramBytes:           maxVram,
				VramPolicy:             defaultVramPolicy,
				LastUpdateNs:           0,
			}
			if err := objs.CgroupQuotaMap.Put(&cgID, &state); err != nil {
				log.Fatalf("pre-populating quota for cgroup %d: %v", cgID, err)
			}
			log.Printf("Pre-populated quota for numeric cgroup %d: refill_rate=%d tokens/sec, max_burst=%d, max_vram=%d bytes, vram_policy=%d", cgID, refill, maxBurst, maxVram, defaultVramPolicy)
		} else {
			// Path or glob pattern target for dynamic watcher
			watcherSpecs = append(watcherSpecs, cgroup.QuotaSpec{
				Target:       target,
				RefillRate:   refill,
				MaxBurst:     maxBurst,
				MaxVramBytes: maxVram,
				VramPolicy:   defaultVramPolicy,
			})
		}
	}

	// Start dynamic cgroup watcher if path-based specs are present
	if len(watcherSpecs) > 0 {
		watcher := cgroup.NewWatcher(objs.CgroupQuotaMap, watcherSpecs)
		watcherCtx, watcherCancel := context.WithCancel(context.Background())
		defer watcherCancel()
		go watcher.Start(watcherCtx, 50*time.Millisecond)
		log.Printf("Started dynamic cgroup watcher for %d spec(s)", len(watcherSpecs))
	}

	// Start Kubernetes workload metadata resolver & annotation quota controller
	resolver := k8s.NewResolver(objs.CgroupQuotaMap, *kubeletPodsURL, *kubeletPodsFile, *kubeletTokenFile)
	resolver.SetDefaultVRAMPolicy(defaultVramPolicy)
	resolver.SetDRAClaimsSource(*draClaimsURL, *draClaimsFile)

	var draDriver *dra.Driver
	if *cdiDir != "" || *draSock != "" {
		draDriver = dra.NewDriver(*cdiDir)
		resolver.SetDRADriver(draDriver)
		log.Printf("Initialized DRA driver with CDI spec directory: %s", *cdiDir)
	}

	if draDriver != nil && *draSock != "" {
		stopSock, err := draDriver.StartSocketServer(*draSock)
		if err != nil {
			log.Fatalf("starting DRA socket server on %s: %v", *draSock, err)
		}
		defer func() {
			_ = stopSock()
		}()
		log.Printf("Started DRA UNIX socket server on %s", *draSock)
	}

	defaultTokensCap := uint64(400000)
	defaultMemCap := uint64(16 * 1024 * 1024 * 1024) // 16 GiB
	effectiveNodeName := *nodeName
	if effectiveNodeName == "" {
		effectiveNodeName = "localhost"
	}

	if *draSliceFile != "" {
		if err := dra.WriteResourceSliceFile(*draSliceFile, effectiveNodeName, defaultTokensCap, defaultMemCap); err != nil {
			log.Printf("warning: failed to write ResourceSlice file %s: %v", *draSliceFile, err)
		} else {
			log.Printf("Published local DRA ResourceSlice file: %s (node: %s)", *draSliceFile, effectiveNodeName)
		}
	}

	if *draSliceURL != "" && *nodeName != "" {
		var token string
		if *kubeletTokenFile != "" {
			if data, err := os.ReadFile(*kubeletTokenFile); err == nil {
				token = strings.TrimSpace(string(data))
			}
		}
		if err := dra.PublishResourceSliceHTTP(*draSliceURL, token, *nodeName, defaultTokensCap, defaultMemCap); err != nil {
			log.Printf("warning: failed to publish ResourceSlice to %s: %v", *draSliceURL, err)
		} else {
			log.Printf("Published DRA ResourceSlice to Kubernetes API: %s (node: %s)", *draSliceURL, *nodeName)
		}
	}

	resolverCtx, resolverCancel := context.WithCancel(context.Background())
	defer resolverCancel()
	go resolver.Start(resolverCtx, 100*time.Millisecond)
	log.Printf("Started Kubernetes workload metadata resolver")

	// Resolve libcuda path with driver auto-discovery
	resolvedLibcuda, err := resolveLibcudaPath(*libcudaPath)
	if err != nil {
		log.Fatalf("resolving libcuda: %v", err)
	}
	log.Printf("Resolved libcuda path: %s", resolvedLibcuda)

	// Open executable and attach uprobes & uretprobes
	ex, err := link.OpenExecutable(resolvedLibcuda)
	if err != nil {
		log.Fatalf("opening executable %s: %v", resolvedLibcuda, err)
	}

	kpLaunch, err := ex.Uprobe("cuLaunchKernel", objs.HandleCuLaunchKernel, nil)
	if err != nil {
		log.Fatalf("attaching uprobe cuLaunchKernel: %v", err)
	}
	defer kpLaunch.Close()

	kpLaunchEx, err := ex.Uprobe("cuLaunchKernelEx", objs.HandleCuLaunchKernelEx, nil)
	if err != nil {
		log.Printf("warning: could not attach uprobe cuLaunchKernelEx: %v", err)
	} else {
		defer kpLaunchEx.Close()
		log.Printf("Successfully attached uprobe cuLaunchKernelEx")
	}

	kpGraph, err := ex.Uprobe("cuGraphLaunch", objs.HandleCuGraphLaunch, nil)
	if err != nil {
		log.Printf("warning: could not attach uprobe cuGraphLaunch: %v", err)
	} else {
		defer kpGraph.Close()
		log.Printf("Successfully attached uprobe cuGraphLaunch")
	}

	kpGraphAddNode, err := ex.Uprobe("cuGraphAddKernelNode", objs.HandleCuGraphAddKernelNode, nil)
	if err != nil {
		log.Printf("warning: could not attach uprobe cuGraphAddKernelNode: %v", err)
	} else {
		defer kpGraphAddNode.Close()
		log.Printf("Successfully attached uprobe cuGraphAddKernelNode")
	}

	kpGraphInst, err := ex.Uprobe("cuGraphInstantiate", objs.HandleCuGraphInstantiate, nil)
	if err != nil {
		log.Printf("warning: could not attach uprobe cuGraphInstantiate: %v", err)
	} else {
		defer kpGraphInst.Close()
		log.Printf("Successfully attached uprobe cuGraphInstantiate")
	}

	kpGraphInstRet, err := ex.Uretprobe("cuGraphInstantiate", objs.HandleCuGraphInstantiateRet, nil)
	if err != nil {
		log.Printf("warning: could not attach uretprobe cuGraphInstantiate: %v", err)
	} else {
		defer kpGraphInstRet.Close()
		log.Printf("Successfully attached uretprobe cuGraphInstantiate")
	}

	kpAlloc, err := ex.Uprobe("cuMemAlloc_v2", objs.HandleCuMemAlloc, nil)
	if err != nil {
		log.Fatalf("attaching uprobe cuMemAlloc_v2: %v", err)
	}
	defer kpAlloc.Close()

	kpAllocRet, err := ex.Uretprobe("cuMemAlloc_v2", objs.HandleCuMemAllocRet, nil)
	if err != nil {
		log.Fatalf("attaching uretprobe cuMemAlloc_v2: %v", err)
	}
	defer kpAllocRet.Close()

	kpFree, err := ex.Uprobe("cuMemFree_v2", objs.HandleCuMemFree, nil)
	if err != nil {
		log.Fatalf("attaching uprobe cuMemFree_v2: %v", err)
	}
	defer kpFree.Close()

	kpAllocAsync, err := ex.Uprobe("cuMemAllocAsync", objs.HandleCuMemAllocAsync, nil)
	if err != nil {
		log.Printf("warning: could not attach uprobe cuMemAllocAsync: %v", err)
	} else {
		defer kpAllocAsync.Close()
		log.Printf("Successfully attached uprobe cuMemAllocAsync")
	}

	kpAllocAsyncRet, err := ex.Uretprobe("cuMemAllocAsync", objs.HandleCuMemAllocAsyncRet, nil)
	if err != nil {
		log.Printf("warning: could not attach uretprobe cuMemAllocAsync: %v", err)
	} else {
		defer kpAllocAsyncRet.Close()
		log.Printf("Successfully attached uretprobe cuMemAllocAsync")
	}

	kpFreeAsync, err := ex.Uprobe("cuMemFreeAsync", objs.HandleCuMemFreeAsync, nil)
	if err != nil {
		log.Printf("warning: could not attach uprobe cuMemFreeAsync: %v", err)
	} else {
		defer kpFreeAsync.Close()
		log.Printf("Successfully attached uprobe cuMemFreeAsync")
	}

	log.Printf("Successfully attached probes to %s (cuLaunchKernel, cuLaunchKernelEx, cuGraphLaunch, cuGraphAddKernelNode, cuGraphInstantiate, cuMemAlloc_v2, cuMemAllocAsync, cuMemFree_v2, cuMemFreeAsync)", resolvedLibcuda)

	pauseTracker := newWallPauseTracker()

	// Ring buffer consumer
	rd, err := ringbuf.NewReader(objs.EventsRingbuf)
	if err != nil {
		log.Fatalf("opening ringbuf reader: %v", err)
	}
	defer rd.Close()

	go func() {
		var record ringbuf.Record
		for {
			err := rd.ReadInto(&record)
			if err != nil {
				if errors.Is(err, ringbuf.ErrClosed) {
					return
				}
				log.Printf("ringbuf read error: %v", err)
				continue
			}

			var ev bpf.ThrottleEvent
			if err := binary.Read(bytes.NewReader(record.RawSample), binary.LittleEndian, &ev); err != nil {
				log.Printf("decoding throttle_event: %v", err)
				continue
			}

			pid := int(ev.Pid)
			sleepDur := time.Duration(ev.RequestedSleepNs) * time.Nanosecond
			pausedAt := time.Now()
			time.AfterFunc(sleepDur, func() {
				_ = syscall.Kill(pid, syscall.SIGCONT)
				pauseTracker.Add(ev.CgroupID, uint64(time.Since(pausedAt).Nanoseconds()))
			})
		}
	}()

	// Register Prometheus metrics collector with K8s label resolver and wall pause tracker
	collector := newBPFCollector(objs.CgroupQuotaMap, resolver, pauseTracker)
	prometheus.MustRegister(collector)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	server := &http.Server{
		Addr:    *metricsAddr,
		Handler: mux,
	}

	go func() {
		log.Printf("Serving Prometheus metrics at http://%s/metrics", *metricsAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// Wait for SIGINT or SIGTERM
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigChan
	log.Printf("Received signal %s, shutting down...", sig)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}
}
