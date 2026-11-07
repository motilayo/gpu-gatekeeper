package main

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/cilium/ebpf"
	"github.com/motilayo/gpu-gatekeeper/internal/bpf"
	"github.com/motilayo/gpu-gatekeeper/internal/k8s"
)

// TenantInfo represents the real-time telemetry and attribution metadata for a single GPU tenant cgroup.
type TenantInfo struct {
	CgroupID               uint64  `json:"cgroup_id"`
	Namespace              string  `json:"namespace"`
	Pod                    string  `json:"pod"`
	Container              string  `json:"container"`
	ResourceClaim          string  `json:"resource_claim"`
	RefillRateTokensPerSec uint64  `json:"refill_rate_tokens_per_sec"`
	MaxBurstTokens         uint64  `json:"max_burst_tokens"`
	CurrentTokens          uint64  `json:"current_tokens"`
	DebtTokens             uint64  `json:"debt_tokens"`
	TotalLaunches          uint64  `json:"total_launches"`
	TotalThrottled         uint64  `json:"total_throttled"`
	ThrottledSeconds       float64 `json:"throttled_seconds"`
	AllocatedVramBytes     uint64  `json:"allocated_vram_bytes"`
	MaxVramBytes           uint64  `json:"max_vram_bytes"`
	VramQuotaExceeded      uint64  `json:"vram_quota_exceeded"`
	VramPolicy             string  `json:"vram_policy"`
}

// TenantsResponse represents the JSON payload returned by GET /api/v1/tenants.
type TenantsResponse struct {
	Timestamp string       `json:"timestamp"`
	Tenants   []TenantInfo `json:"tenants"`
}

func handleTenantsAPI(quotaMap *ebpf.Map, resolver *k8s.Resolver, pauseTracker *wallPauseTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var tenants []TenantInfo

		if quotaMap != nil {
			var cgroupID uint64
			var state bpf.CgroupQuotaState

			iter := quotaMap.Iterate()
			for iter.Next(&cgroupID, &state) {
				var meta k8s.WorkloadMeta
				if resolver != nil {
					meta = resolver.LookupMeta(cgroupID)
				}

				throttledNs := uint64(0)
				if pauseTracker != nil {
					throttledNs = pauseTracker.Get(cgroupID)
				}
				if throttledNs < state.TotalThrottledNs {
					throttledNs = state.TotalThrottledNs
				}

				policyStr := "audit"
				switch state.VramPolicy {
				case 1:
					policyStr = "throttle"
				case 2:
					policyStr = "kill"
				}

				tenants = append(tenants, TenantInfo{
					CgroupID:               cgroupID,
					Namespace:              meta.Namespace,
					Pod:                    meta.Pod,
					Container:              meta.Container,
					ResourceClaim:          meta.ResourceClaim,
					RefillRateTokensPerSec: state.RefillRateTokensPerSec,
					MaxBurstTokens:         state.MaxBurstTokens,
					CurrentTokens:          state.CurrentTokens,
					DebtTokens:             state.DebtTokens,
					TotalLaunches:          state.TotalLaunches,
					TotalThrottled:         state.TotalThrottled,
					ThrottledSeconds:       math.Round((float64(throttledNs)/1e9)*1000) / 1000,
					AllocatedVramBytes:     state.AllocatedVramBytes,
					MaxVramBytes:           state.MaxVramBytes,
					VramQuotaExceeded:      state.VramQuotaExceeded,
					VramPolicy:             policyStr,
				})
			}
			_ = iter.Err()
		}

		// Sort tenants by cgroup_id ascending for deterministic output
		sort.Slice(tenants, func(i, j int) bool {
			return tenants[i].CgroupID < tenants[j].CgroupID
		})

		if tenants == nil {
			tenants = []TenantInfo{}
		}

		resp := TenantsResponse{
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Tenants:   tenants,
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func handleDashboardUI() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(dashboardHTML))
	}
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>G-Gate Live GPU QoS & Attribution Console</title>
  <style>
    :root {
      --bg-dark: #0a0e17;
      --card-bg: #111827;
      --card-border: #1f293d;
      --text-main: #f1f5f9;
      --text-muted: #94a3b8;
      --text-dim: #64748b;
      --accent-green: #10b981;
      --accent-cyan: #06b6d4;
      --accent-purple: #a855f7;
      --accent-amber: #f59e0b;
      --accent-red: #ef4444;
      --accent-blue: #3b82f6;
    }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      background-color: var(--bg-dark);
      color: var(--text-main);
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      min-height: 100vh;
      display: flex;
      flex-direction: column;
    }
    header {
      background-color: #0f172a;
      border-bottom: 1px solid var(--card-border);
      padding: 0.85rem 1.5rem;
      display: flex;
      justify-content: space-between;
      align-items: center;
      position: sticky;
      top: 0;
      z-index: 50;
    }
    .brand {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      font-weight: 700;
      font-size: 1.15rem;
      letter-spacing: -0.025em;
    }
    .brand svg {
      width: 28px;
      height: 28px;
      fill: var(--accent-cyan);
    }
    .version-tag {
      background: rgba(6, 182, 212, 0.15);
      color: var(--accent-cyan);
      border: 1px solid rgba(6, 182, 212, 0.3);
      padding: 0.15rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 600;
    }
    .nav-actions {
      display: flex;
      align-items: center;
      gap: 1rem;
    }
    .pulse-pill {
      display: inline-flex;
      align-items: center;
      gap: 0.4rem;
      background: rgba(16, 185, 129, 0.12);
      color: var(--accent-green);
      border: 1px solid rgba(16, 185, 129, 0.3);
      padding: 0.25rem 0.65rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
    }
    .dot {
      width: 8px;
      height: 8px;
      background-color: var(--accent-green);
      border-radius: 50%;
      box-shadow: 0 0 8px var(--accent-green);
      animation: pulse 1.5s infinite;
    }
    @keyframes pulse {
      0% { opacity: 0.4; }
      50% { opacity: 1; transform: scale(1.15); }
      100% { opacity: 0.4; }
    }
    .links a {
      color: var(--text-muted);
      text-decoration: none;
      font-size: 0.85rem;
      padding: 0.35rem 0.65rem;
      border-radius: 6px;
      transition: all 0.15s ease;
      border: 1px solid transparent;
    }
    .links a:hover {
      color: var(--text-main);
      background-color: rgba(255, 255, 255, 0.05);
      border-color: var(--card-border);
    }
    main {
      padding: 1.5rem;
      max-width: 1440px;
      width: 100%;
      margin: 0 auto;
      flex: 1;
    }
    .stats-grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
      gap: 1rem;
      margin-bottom: 1.5rem;
    }
    .stat-card {
      background: var(--card-bg);
      border: 1px solid var(--card-border);
      border-radius: 10px;
      padding: 1.25rem;
      position: relative;
      overflow: hidden;
    }
    .stat-card::before {
      content: "";
      position: absolute;
      top: 0;
      left: 0;
      right: 0;
      height: 3px;
      background: var(--stat-bar, var(--accent-cyan));
    }
    .stat-label {
      font-size: 0.8rem;
      font-weight: 600;
      color: var(--text-muted);
      text-transform: uppercase;
      letter-spacing: 0.05em;
      margin-bottom: 0.35rem;
    }
    .stat-value {
      font-size: 1.85rem;
      font-weight: 700;
      letter-spacing: -0.03em;
    }
    .stat-sub {
      font-size: 0.75rem;
      color: var(--text-dim);
      margin-top: 0.35rem;
    }
    .table-container {
      background: var(--card-bg);
      border: 1px solid var(--card-border);
      border-radius: 10px;
      overflow-x: auto;
      box-shadow: 0 4px 20px rgba(0, 0, 0, 0.25);
    }
    .table-header-bar {
      padding: 1rem 1.25rem;
      border-bottom: 1px solid var(--card-border);
      display: flex;
      justify-content: space-between;
      align-items: center;
    }
    .table-title {
      font-size: 1rem;
      font-weight: 600;
      letter-spacing: -0.01em;
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }
    table {
      width: 100%;
      border-collapse: collapse;
      text-align: left;
      font-size: 0.85rem;
    }
    th {
      background: rgba(15, 23, 42, 0.6);
      color: var(--text-muted);
      font-weight: 600;
      text-transform: uppercase;
      font-size: 0.7rem;
      letter-spacing: 0.05em;
      padding: 0.85rem 1rem;
      border-bottom: 1px solid var(--card-border);
      white-space: nowrap;
    }
    td {
      padding: 0.85rem 1rem;
      border-bottom: 1px solid var(--card-border);
      vertical-align: middle;
      color: var(--text-main);
    }
    tr:last-child td {
      border-bottom: none;
    }
    tr:hover td {
      background: rgba(255, 255, 255, 0.02);
    }
    .badge {
      display: inline-block;
      padding: 0.15rem 0.45rem;
      border-radius: 4px;
      font-size: 0.7rem;
      font-weight: 600;
      font-family: monospace;
    }
    .badge-claim {
      background: rgba(168, 85, 247, 0.15);
      color: #c084fc;
      border: 1px solid rgba(168, 85, 247, 0.3);
    }
    .badge-policy-audit {
      background: rgba(16, 185, 129, 0.15);
      color: var(--accent-green);
      border: 1px solid rgba(16, 185, 129, 0.3);
    }
    .badge-policy-throttle {
      background: rgba(245, 158, 11, 0.15);
      color: var(--accent-amber);
      border: 1px solid rgba(245, 158, 11, 0.3);
    }
    .badge-policy-kill {
      background: rgba(239, 68, 68, 0.15);
      color: var(--accent-red);
      border: 1px solid rgba(239, 68, 68, 0.3);
    }
    .pod-meta {
      font-weight: 600;
      color: #e2e8f0;
      margin-bottom: 0.2rem;
    }
    .cg-id {
      font-family: monospace;
      font-size: 0.75rem;
      color: var(--text-dim);
    }
    .bar-bg {
      background: rgba(255, 255, 255, 0.08);
      border-radius: 4px;
      height: 6px;
      overflow: hidden;
      margin-top: 0.3rem;
      width: 110px;
    }
    .bar-fill {
      height: 100%;
      background: var(--accent-cyan);
      transition: width 0.3s ease;
    }
    .empty-state {
      padding: 3rem;
      text-align: center;
      color: var(--text-dim);
      font-size: 0.95rem;
    }
    footer {
      border-top: 1px solid var(--card-border);
      padding: 0.75rem 1.5rem;
      text-align: center;
      color: var(--text-dim);
      font-size: 0.75rem;
    }
  </style>
</head>
<body>
  <header>
    <div class="brand">
      <svg viewBox="0 0 24 24"><path d="M4 6a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6zm2 0v12h12V6H6zm2 2h8v2H8V8zm0 4h8v2H8v-2zm0 4h5v2H8v-2z"/></svg>
      <span>G-Gate Live GPU QoS & Attribution Console</span>
      <span class="version-tag">v0.5</span>
    </div>
    <div class="nav-actions">
      <div class="pulse-pill">
        <span class="dot"></span>
        <span>Live</span>
      </div>
      <div class="links">
        <a href="/metrics" target="_blank">/metrics (Prometheus)</a>
        <a href="/api/v1/tenants" target="_blank">/api/v1/tenants (JSON)</a>
      </div>
    </div>
  </header>

  <main>
    <div class="stats-grid">
      <div class="stat-card" style="--stat-bar: var(--accent-cyan);">
        <div class="stat-label">Active GPU Cgroups</div>
        <div class="stat-value" id="card-active-tenants">0</div>
        <div class="stat-sub">Tracked eBPF Workload Slices</div>
      </div>
      <div class="stat-card" style="--stat-bar: var(--accent-green);">
        <div class="stat-label">Total GPU Throughput</div>
        <div class="stat-value" id="card-total-iops">0.0</div>
        <div class="stat-sub">Kernel Launches/sec (IOPS)</div>
      </div>
      <div class="stat-card" style="--stat-bar: var(--accent-purple);">
        <div class="stat-label">Active Allocated VRAM</div>
        <div class="stat-value" id="card-total-vram">0 B</div>
        <div class="stat-sub">Attributed Memory via cuMemAlloc</div>
      </div>
      <div class="stat-card" style="--stat-bar: var(--accent-amber);">
        <div class="stat-label">VRAM Quota Violations</div>
        <div class="stat-value" id="card-total-violations">0</div>
        <div class="stat-sub">Cumulative Exceeded Allocation Attempts</div>
      </div>
    </div>

    <div class="table-container">
      <div class="table-header-bar">
        <div class="table-title">Active GPU Workload Tenants</div>
        <div style="font-size: 0.75rem; color: var(--text-dim);" id="last-updated">Updating...</div>
      </div>
      <table id="tenants-table">
        <thead>
          <tr>
            <th>Tenant Identity</th>
            <th>Throughput (IOPS)</th>
            <th>Proportional Share</th>
            <th>Token Bucket</th>
            <th>QoS Throttle Pauses</th>
            <th>VRAM Allocation</th>
            <th>Quota Policy</th>
          </tr>
        </thead>
        <tbody id="tenants-body">
          <tr><td colspan="7" class="empty-state">Connecting to eBPF governor on :9090...</td></tr>
        </tbody>
      </table>
    </div>
  </main>

  <footer>
    G-Gate v2 • Zero-Injection eBPF GPU Attribution & QoS Governor • Linux 6.8+ / Kubernetes 1.28-1.34 DRA
  </footer>

  <script>
    let prevData = {};
    let prevTimestamp = null;

    function formatBytes(bytes) {
      if (bytes === 0) return '0 B';
      const k = 1024;
      const sizes = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
      const i = Math.floor(Math.log(bytes) / Math.log(k));
      return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
    }

    async function refresh() {
      try {
        const res = await fetch('/api/v1/tenants');
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const data = await res.json();

        const now = new Date(data.timestamp || Date.now()).getTime();
        const dt = prevTimestamp ? (now - prevTimestamp) / 1000 : 1.0;

        let totalIOPS = 0;
        let totalVRAM = 0;
        let totalViolations = 0;

        const currentMap = {};
        const calculatedTenants = [];

        data.tenants.forEach(t => {
          totalVRAM += t.allocated_vram_bytes;
          totalViolations += t.vram_quota_exceeded;

          let instantIOPS = 0;
          if (prevData[t.cgroup_id] && dt > 0) {
            const dLaunches = t.total_launches - prevData[t.cgroup_id].total_launches;
            instantIOPS = Math.max(0, dLaunches / dt);
          } else if (t.refill_rate_tokens_per_sec > 0) {
            instantIOPS = t.refill_rate_tokens_per_sec / 32;
          }
          totalIOPS += instantIOPS;

          currentMap[t.cgroup_id] = { total_launches: t.total_launches };
          calculatedTenants.push({ ...t, iops: instantIOPS });
        });

        prevData = currentMap;
        prevTimestamp = now;

        document.getElementById('card-active-tenants').innerText = data.tenants.length;
        document.getElementById('card-total-iops').innerText = totalIOPS.toLocaleString(undefined, { maximumFractionDigits: 1 });
        document.getElementById('card-total-vram').innerText = formatBytes(totalVRAM);
        document.getElementById('card-total-violations').innerText = totalViolations;
        document.getElementById('last-updated').innerText = 'Last update: ' + new Date().toLocaleTimeString();

        const tbody = document.getElementById('tenants-body');
        if (calculatedTenants.length === 0) {
          tbody.innerHTML = '<tr><td colspan="7" class="empty-state">No active GPU cgroups enrolled yet. Waiting for workloads...</td></tr>';
          return;
        }

        let html = '';
        calculatedTenants.forEach(t => {
          const sharePct = totalIOPS > 0 ? ((t.iops / totalIOPS) * 100).toFixed(1) : '0.0';
          const vramPct = t.max_vram_bytes > 0 ? Math.min(100, (t.allocated_vram_bytes / t.max_vram_bytes) * 100).toFixed(1) : 0;
          const vramBarColor = vramPct >= 100 ? 'var(--accent-red)' : vramPct > 80 ? 'var(--accent-amber)' : 'var(--accent-cyan)';

          let podIdentity = t.pod ? (t.namespace + '/' + t.pod) : ('cgroup-' + t.cgroup_id);
          if (t.container) podIdentity += ' (' + t.container + ')';

          const claimBadge = t.resource_claim ? '<span class="badge badge-claim">' + t.resource_claim + '</span>' : '';
          const policyClass = 'badge-policy-' + (t.vram_policy || 'audit');

          html += '<tr>' +
            '<td>' +
              '<div class="pod-meta">' + podIdentity + ' ' + claimBadge + '</div>' +
              '<div class="cg-id">cgroup inode: ' + t.cgroup_id + '</div>' +
            '</td>' +
            '<td><strong>' + t.iops.toFixed(1) + '</strong> <span style="font-size:0.75rem;color:var(--text-dim)">IOPS</span></td>' +
            '<td>' +
              '<div style="font-weight:600">' + sharePct + '%</div>' +
              '<div class="bar-bg"><div class="bar-fill" style="width:' + sharePct + '%"></div></div>' +
            '</td>' +
            '<td>' +
              '<div>' + t.refill_rate_tokens_per_sec.toLocaleString() + ' <span style="font-size:0.75rem;color:var(--text-dim)">tok/s</span></div>' +
              '<div style="font-size:0.75rem;color:var(--text-dim)">cur: ' + t.current_tokens + ' / max: ' + t.max_burst_tokens + (t.debt_tokens > 0 ? (' (debt: ' + t.debt_tokens + ')') : '') + '</div>' +
            '</td>' +
            '<td>' +
              '<div>' + t.total_throttled.toLocaleString() + ' <span style="font-size:0.75rem;color:var(--text-dim)">events</span></div>' +
              '<div style="font-size:0.75rem;color:var(--accent-amber)">' + t.throttled_seconds.toFixed(3) + ' s paused</div>' +
            '</td>' +
            '<td>' +
              '<div>' + formatBytes(t.allocated_vram_bytes) + ' ' + (t.max_vram_bytes > 0 ? ('/ ' + formatBytes(t.max_vram_bytes)) : '') + '</div>' +
              (t.max_vram_bytes > 0 ? ('<div class="bar-bg"><div class="bar-fill" style="width:' + vramPct + '%;background:' + vramBarColor + '"></div></div>') : '') +
            '</td>' +
            '<td>' +
              '<span class="badge ' + policyClass + '">' + (t.vram_policy || 'audit').toUpperCase() + '</span>' +
              (t.vram_quota_exceeded > 0 ? (' <span style="color:var(--accent-red);font-size:0.75rem;font-weight:600">(' + t.vram_quota_exceeded + ' over)</span>') : '') +
            '</td>' +
          '</tr>';
        });

        tbody.innerHTML = html;
      } catch (err) {
        console.error('Refresh error:', err);
      }
    }

    refresh();
    setInterval(refresh, 1000);
  </script>
</body>
</html>`
