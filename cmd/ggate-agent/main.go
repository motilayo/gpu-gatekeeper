package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
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

type bpfCollector struct {
	quotaMap            *ebpf.Map
	resolver            *k8s.Resolver
	launchesDesc        *prometheus.Desc
	throttledDesc       *prometheus.Desc
	throttledSecDesc    *prometheus.Desc
	vramDesc            *prometheus.Desc
	vramQuotaDesc       *prometheus.Desc
	vramQuotaExceedDesc *prometheus.Desc
}

func newBPFCollector(quotaMap *ebpf.Map, resolver *k8s.Resolver) *bpfCollector {
	labels := []string{"cgroup_id", "namespace", "pod", "container"}
	return &bpfCollector{
		quotaMap: quotaMap,
		resolver: resolver,
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

		ch <- prometheus.MustNewConstMetric(
			c.launchesDesc,
			prometheus.CounterValue,
			float64(state.TotalLaunches),
			cgStr, meta.Namespace, meta.Pod, meta.Container,
		)
		ch <- prometheus.MustNewConstMetric(
			c.throttledDesc,
			prometheus.CounterValue,
			float64(state.TotalThrottled),
			cgStr, meta.Namespace, meta.Pod, meta.Container,
		)
		ch <- prometheus.MustNewConstMetric(
			c.throttledSecDesc,
			prometheus.CounterValue,
			float64(state.TotalThrottledNs)/1e9,
			cgStr, meta.Namespace, meta.Pod, meta.Container,
		)
		ch <- prometheus.MustNewConstMetric(
			c.vramDesc,
			prometheus.GaugeValue,
			float64(state.AllocatedVramBytes),
			cgStr, meta.Namespace, meta.Pod, meta.Container,
		)
		ch <- prometheus.MustNewConstMetric(
			c.vramQuotaDesc,
			prometheus.GaugeValue,
			float64(state.MaxVramBytes),
			cgStr, meta.Namespace, meta.Pod, meta.Container,
		)
		ch <- prometheus.MustNewConstMetric(
			c.vramQuotaExceedDesc,
			prometheus.CounterValue,
			float64(state.VramQuotaExceeded),
			cgStr, meta.Namespace, meta.Pod, meta.Container,
		)
	}
	if err := iter.Err(); err != nil {
		log.Printf("error iterating cgroup_quota_map: %v", err)
	}
}

func main() {
	var quotas quotaList
	flag.Var(&quotas, "quota", "Cgroup quota in format target:refill_rate:max_burst (can be specified multiple times, target can be numeric inode or path/glob)")
	libcudaPath := flag.String("libcuda", "./build/libcuda.so", "Path to libcuda.so mock library")
	metricsAddr := flag.String("metrics-addr", ":9090", "Prometheus metrics HTTP address")
	kubeletPodsURL := flag.String("kubelet-pods-url", "", "HTTP URL returning Kubelet /pods JSON")
	kubeletPodsFile := flag.String("kubelet-pods-file", "", "Path to local JSON file with Kubelet /pods format")
	kubeletTokenFile := flag.String("kubelet-token-file", "/var/run/secrets/kubernetes.io/serviceaccount/token", "Path to ServiceAccount bearer token for Kubelet HTTPS :10250/pods")
	flag.Parse()

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
		if len(parts) != 3 {
			log.Fatalf("invalid --quota format %q, expected target:refill_rate:max_burst", q)
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

		// Check if target is a numeric cgroup_id inode
		cgID, err := strconv.ParseUint(target, 10, 64)
		if err == nil {
			state := bpf.CgroupQuotaState{
				RefillRateTokensPerSec: refill,
				MaxBurstTokens:         maxBurst,
				CurrentTokens:          maxBurst,
				LastUpdateNs:           0,
			}
			if err := objs.CgroupQuotaMap.Put(&cgID, &state); err != nil {
				log.Fatalf("pre-populating quota for cgroup %d: %v", cgID, err)
			}
			log.Printf("Pre-populated quota for numeric cgroup %d: refill_rate=%d tokens/sec, max_burst=%d", cgID, refill, maxBurst)
		} else {
			// Path or glob pattern target for dynamic watcher
			watcherSpecs = append(watcherSpecs, cgroup.QuotaSpec{
				Target:     target,
				RefillRate: refill,
				MaxBurst:   maxBurst,
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
	resolverCtx, resolverCancel := context.WithCancel(context.Background())
	defer resolverCancel()
	go resolver.Start(resolverCtx, 100*time.Millisecond)
	log.Printf("Started Kubernetes workload metadata resolver")

	// Open executable and attach uprobes & uretprobes
	ex, err := link.OpenExecutable(*libcudaPath)
	if err != nil {
		log.Fatalf("opening executable %s: %v", *libcudaPath, err)
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

	log.Printf("Successfully attached probes to %s (cuLaunchKernel, cuLaunchKernelEx, cuMemAlloc_v2, cuMemAlloc_v2 ret, cuMemFree_v2)", *libcudaPath)

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
			time.AfterFunc(sleepDur, func() {
				_ = syscall.Kill(pid, syscall.SIGCONT)
			})
		}
	}()

	// Register Prometheus metrics collector with K8s label resolver
	collector := newBPFCollector(objs.CgroupQuotaMap, resolver)
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
