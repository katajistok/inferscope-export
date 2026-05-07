# Inferscope

**Real-time GPU and LLM inference observability for Prometheus.**

Inferscope is a monitoring system for GPU nodes running AI inference workloads. It automatically discovers inference processes, correlates GPU hardware metrics with token-level data from the running models, and exposes everything as Prometheus metrics — without requiring any changes to the inference runtime.

```
GPU Hardware (NVML / rocm-smi / intel_gpu_top)
        +
Inference processes (/proc fd scanning)
        +
Token metrics (opportunistic /metrics scrape)
        ↓
  Axon  :9101/metrics
        ↓
  Prometheus / vmagent
        ↓
  Thanos → Grafana
```

---

## Components

| Component | Role |
|-----------|------|
| **Axon** | Node agent — collects GPU hardware counters, discovers inference processes, scrapes token metrics. Runs as a systemd service, Docker container, or Kubernetes DaemonSet. |
| **Inferscope** | The project as a whole — Axon + Grafana dashboards + deployment manifests. |

---

## Features

- **Runtime-agnostic token collection** — discovers any inference process via `/proc` fd scanning, identifies its listening ports by cross-referencing socket inodes (correct across network namespaces — no false positives from shared host ports), and probes for a Prometheus `/metrics` endpoint. Works with vLLM, llama.cpp server, NVIDIA NIM, TGI, and anything else that exposes standard Prometheus metrics.
- **AMD + NVIDIA + Intel support** — AMD via `rocm-smi`, NVIDIA via NVML (`go-nvml`), Intel Arc/Xe via `intel_gpu_top` and sysfs. Mixed-vendor nodes supported.
- **No runtime modification required** — Axon is a passive observer. Zero changes to your inference stack.
- **Per-process VRAM tracking** — NVIDIA NVML exposes per-PID VRAM usage directly.
- **Efficiency metric** — tokens/s per watt, computed automatically from token throughput and GPU power draw. Note: some mobile/workstation GPUs (e.g. RTX A1000) do not expose power draw via NVML — `gpu_power_watts` will be 0 on those devices and efficiency metrics will be unavailable.
- **Single binary** — Go, no runtime dependencies, ~15 MB binary.
- **Multiple deployment modes** — bare metal (systemd), single-host Docker Compose, or Kubernetes DaemonSet.

---

## Token metric support by runtime

| Runtime | Token metrics | Notes |
|---------|--------------|-------|
| **vLLM** | ✅ Full | `/metrics` on port 8000 |
| **NVIDIA NIM** | ✅ Full | `/v1/metrics` — passes through vLLM metrics unchanged |
| **llama.cpp server** | ✅ Full | `/metrics` — requires `--metrics` flag at startup |
| **TGI** | ✅ Full | `/metrics` on port 3000 |
| **Ollama** | ⚠️ Hardware only | No Prometheus endpoint — process detected, GPU metrics collected, token counts unavailable |
| **Custom / unknown** | ⚠️ Best-effort | Any runtime exposing Prometheus-format `/metrics` will be auto-detected |

---

## Metrics reference

### GPU hardware

| Metric | Labels | Description |
|--------|--------|-------------|
| `gpu_utilization_percent` | `gpu_id` `vendor` `gpu_name` `node` | Compute/render engine utilization % |
| `gpu_memory_used_mib` | `gpu_id` `vendor` `gpu_name` `node` | VRAM used in MiB |
| `gpu_memory_total_mib` | `gpu_id` `vendor` `gpu_name` `node` | VRAM total in MiB |
| `gpu_power_watts` | `gpu_id` `vendor` `gpu_name` `node` | Power draw in watts |
| `gpu_temperature_celsius` | `gpu_id` `vendor` `gpu_name` `node` | GPU temperature |
| `gpu_clock_mhz` | `gpu_id` `vendor` `gpu_name` `node` | Core clock speed |

### Process / workload

| Metric | Labels | Description |
|--------|--------|-------------|
| `gpu_process_running` | `gpu_id` `vendor` `gpu_name` `node` `pid` `model` `runtime` | 1 if inference process active |
| `gpu_process_memory_mib` | `gpu_id` `vendor` `gpu_name` `node` `pid` `model` `runtime` | VRAM used by process (NVIDIA only via NVML) |

### Token metrics

| Metric | Labels | Description |
|--------|--------|-------------|
| `gpu_inference_prompt_tokens_total` | `gpu_id` `vendor` `node` `pid` `model` `runtime` `source` | Prompt tokens processed (counter) |
| `gpu_inference_generated_tokens_total` | `gpu_id` `vendor` `node` `pid` `model` `runtime` `source` | Tokens generated (counter) |
| `gpu_inference_tokens_per_second` | `gpu_id` `vendor` `node` `pid` `model` `runtime` `source` | Current generation throughput |
| `gpu_inference_tokens_per_watt` | `gpu_id` `vendor` `node` `pid` `model` `runtime` `source` | Efficiency: tok/s ÷ watts |
| `gpu_inference_raw_metric` | `gpu_id` `vendor` `node` `pid` `model` `runtime` `source` `metric_name` | All token-related raw metrics scraped verbatim |
| `gpu_inference_token_source` | `gpu_id` `vendor` `node` `pid` `model` `runtime` `source` `scrape_source` | 1 with label indicating how token metrics were collected |

**Label values:**
- `vendor`: `nvidia`, `amd`, or `intel`
- `runtime`: `vllm`, `nim`, `llama.cpp`, `tgi`, `ollama`, or `unknown`
- `source`: `prometheus`, `ollama_api`, or `none`
- `cluster`: injected by vmagent relabeling or Kubernetes ServiceMonitor — identifies the cluster in multi-cluster deployments

---

## Deployment

### Option 1 — Bare metal (systemd)

Best for dedicated GPU servers running Ubuntu/Debian directly.

```bash
git clone https://github.com/youorg/inferscope
cd inferscope
make build
make systemd-install   # copies binary + unit file, reloads systemd
make systemd-start     # enables and starts axon
make systemd-logs      # follow logs
```

Requires root or `CAP_SYS_PTRACE + CAP_DAC_READ_SEARCH`.

Start a local Prometheus + Grafana stack alongside:

```bash
make local-up
# Grafana    → http://localhost:3000  (admin / admin)
# Prometheus → http://localhost:9090
```

---

### Option 2 — Single-host Docker Compose

Runs Axon, Prometheus, and Grafana together in containers on one machine.
Axon uses `pid: host` and mounts `/proc`, `/sys`, and `/dev` so it has full
visibility into host GPU processes — identical to bare metal.

```bash
cd deploy/single-host
docker compose up -d

# Grafana    → http://localhost:3000  (admin / admin)
# Prometheus → http://localhost:9090
# Axon       → http://localhost:9101/metrics
```

Stop (keeps data):
```bash
docker compose down
```

Stop and delete all metric history:
```bash
docker compose down -v
```

---

### Option 3 — Kubernetes DaemonSet

Runs one Axon pod per node. Supports two hostPID modes — see comments in
`deploy/kubernetes/daemonset.yaml` for details.

```bash
# Apply all manifests
kubectl apply -f deploy/kubernetes/namespace.yaml
kubectl apply -f deploy/kubernetes/rbac.yaml
kubectl apply -f deploy/kubernetes/daemonset.yaml

# Check pods are running on each node
kubectl get pods -n inferscope -o wide

# If using Prometheus Operator (kube-prometheus-stack)
kubectl apply -f deploy/kubernetes/service-monitor.yaml

# If using vmagent — add to your vmagent scrape config:
# deploy/kubernetes/vmagent-scrape.yml
```

Before deploying to multiple clusters, update the `cluster` label in
`daemonset.yaml` and `service-monitor.yaml` (or `vmagent-scrape.yml`) to
identify each cluster uniquely in Grafana.

---

### Start your inference runtime

Axon discovers inference processes automatically within one poll cycle (default 5s).
For token metrics, the runtime must expose a Prometheus `/metrics` endpoint:

```bash
# llama.cpp server — the --metrics flag is required
llama-server \
  --hf-repo ggml-org/gemma-3-4b-it-GGUF \
  --port 8080 \
  --metrics

# vLLM
vllm serve google/gemma-3-4b-it --port 8000

# NVIDIA NIM — exposes /v1/metrics automatically
docker run --gpus all --rm -p 8000:8000 nvcr.io/nim/meta/llama-3.1-8b-instruct:latest
```

---

## Configuration flags

| Flag | Default | Description |
|------|---------|-------------|
| `--listen-address` | `:9101` | Address to expose `/metrics` on |
| `--poll-interval` | `5s` | Process discovery + token scrape interval |
| `--hw-interval` | `2s` | Hardware counter poll interval |
| `--amd` | `true` | Enable AMD GPU collection (requires `rocm-smi` in PATH) |
| `--nvidia` | `true` | Enable NVIDIA GPU collection (requires NVML) |
| `--intel` | `true` | Enable Intel GPU collection (requires `intel_gpu_top` in PATH) |

---

## vmagent scrape config (static, non-Kubernetes)

```yaml
scrape_configs:
  - job_name: "axon"
    static_configs:
      - targets: ["<node-ip>:9101"]
    relabel_configs:
      - target_label: cluster
        replacement: "my-cluster"
```

> **Note:** Do not add a relabeling rule that extracts `node` from
> `__address__`. Axon already sets the `node` label to the machine hostname.
> Overwriting it with the scrape address would replace the hostname with an
> IP or `localhost`.

For Kubernetes, use `deploy/kubernetes/vmagent-scrape.yml` instead.

---

## Grafana dashboards

Five dashboards are included in `dashboards/` and auto-provisioned when using
the single-host or local deployment stacks. All dashboards share three
cascading dropdowns: **Cluster → Node → GPU**.

| Dashboard | File | Purpose | Default range |
|-----------|------|---------|---------------|
| **Overview** | `overview.json` | Cross-cluster summary — throughput, utilization, active models, efficiency | 6h |
| **Token Consumption** | `tokens.json` | Token usage with 1h/6h/12h/24h fixed-window table, trend charts, per-model breakdown | 24h |
| **GPU Hardware** | `gpu-hardware.json` | Raw hardware — utilization, VRAM, power, temperature, clock per GPU | 3h |
| **Power & Efficiency** | `power-efficiency.json` | Tokens per watt, power draw ranking, estimated kWh, tokens per kWh | 24h |
| **GPU Utilization & Health** | `utilization.json` | Hardware ROI view — fleet average vs 75% target, VRAM, per-GPU table with target met/not met | 24h |

The **Utilization & Health** dashboard is the primary one for organizational
reporting. It shows at a glance how many GPUs are meeting the utilization
target, which nodes are underperforming, and includes a table with a
"target met" column (✓ / ✗) suitable for management reporting.

The **Token Consumption** dashboard token table always shows 1h / 6h / 12h / 24h
fixed windows regardless of the time range picker, so standard comparison
windows are always visible. The stat tiles and trend charts respond to the
time picker.

### Loading dashboards manually

If not using the provisioned stack, import JSON files directly in Grafana:
**Dashboards → Import → Upload JSON file**.



```promql
# ── Token consumption ──────────────────────────────────────────────────────

# Current token throughput per model
gpu_inference_tokens_per_second

# Total tokens generated — last hour per model
sum by (model) (increase(gpu_inference_generated_tokens_total[1h]))

# Total tokens generated — last 24h per node
sum by (node) (increase(gpu_inference_generated_tokens_total[24h]))

# Total tokens — last 24h per cluster
sum by (cluster) (increase(gpu_inference_generated_tokens_total[24h]))

# Token rate over time (tokens per minute), per node
sum by (node) (rate(gpu_inference_generated_tokens_total[$__rate_interval]) * 60)


# ── GPU utilization ────────────────────────────────────────────────────────

# Current utilization per GPU
gpu_utilization_percent

# Average utilization per node over last 24h
avg by (node) (avg_over_time(gpu_utilization_percent[24h]))

# GPUs currently below the 75% utilization target
avg by (node, gpu_id) (avg_over_time(gpu_utilization_percent[24h])) < 75

# Fleet average utilization
avg(avg_over_time(gpu_utilization_percent[$__range]))

# GPU utilization correlated with running model
gpu_utilization_percent * on(node, gpu_id) group_left(model) gpu_process_running


# ── Memory ────────────────────────────────────────────────────────────────

# VRAM usage percentage per GPU
gpu_memory_used_mib / gpu_memory_total_mib * 100

# VRAM used in MiB per GPU
gpu_memory_used_mib

# GPUs with high VRAM but low compute — model loaded but idle
(gpu_memory_used_mib / gpu_memory_total_mib * 100) > 70
  and gpu_utilization_percent < 10


# ── Power and efficiency ───────────────────────────────────────────────────

# Tokens per watt per model
gpu_inference_tokens_per_watt

# Average power draw per node
avg by (node) (gpu_power_watts)

# Estimated energy consumed — selected period (kWh)
sum(avg_over_time(gpu_power_watts[$__range])) * ($__range_s / 3600) / 1000

# Tokens per kWh — overall efficiency
sum(increase(gpu_inference_generated_tokens_total[$__range]))
  /
(sum(avg_over_time(gpu_power_watts[$__range])) * ($__range_s / 3600) / 1000)
```

---

## Repository structure

```
inferscope/
├── cmd/axon/                   # Axon binary entrypoint
├── internal/
│   ├── discovery/              # /proc fd scanner with sysfs vendor detection
│   ├── collector/
│   │   ├── common/             # Shared DeviceMetrics struct + HardwareCollector interface
│   │   ├── amd/                # AMD — rocm-smi JSON polling
│   │   ├── nvidia/             # NVIDIA — NVML via go-nvml
│   │   ├── intel/              # Intel — intel_gpu_top + sysfs hwmon
│   │   └── tokens/             # Runtime-agnostic /metrics scraper
│   └── exporter/               # Prometheus exporter — vendor-generic loop
├── deploy/
│   ├── systemd/                # axon.service unit file
│   ├── docker/                 # Dockerfile + Axon-only docker-compose
│   ├── single-host/            # Axon + Prometheus + Grafana on one machine
│   ├── local/                  # Dev stack (use alongside systemd Axon)
│   └── kubernetes/             # DaemonSet, RBAC, ServiceMonitor, vmagent config
├── dashboards/                 # Grafana dashboard JSON files
├── Makefile
├── CHANGELOG.md
└── README.md
```

---

## Uninstallation

### Bare metal (systemd)

```bash
make uninstall
# or manually:
sudo systemctl disable --now axon
sudo rm /usr/local/bin/axon /etc/systemd/system/axon.service
sudo systemctl daemon-reload
```

### Single-host Docker Compose

```bash
cd deploy/single-host
docker compose down        # stop, keep metric data
docker compose down -v     # stop, delete all metric data
docker rmi axon:latest     # remove the image
```

### Local dev stack

```bash
make local-down            # stop, keep data
make local-purge           # stop, delete all data
```

### Remove everything (bare metal + local dev stack)

```bash
make purge-all
```

### Kubernetes

```bash
kubectl delete -f deploy/kubernetes/
```

---

## Requirements

- Linux (uses `/proc` and `/sys/class/drm`)
- Root or `CAP_SYS_PTRACE + CAP_DAC_READ_SEARCH`
- **AMD:** `rocm-smi` in PATH (ROCm 5.x or 6.x)
- **NVIDIA:** NVML — ships with any NVIDIA driver ≥ 450
- **Intel:** `intel_gpu_top` — `apt install intel-gpu-tools`; root or `CAP_PERFMON` (kernel ≥ 5.8) for perf counters
- Go 1.22+ to build

---

## Roadmap

- [ ] Kubernetes discovery mode (`--discovery=kubernetes`) — list inference pods via K8s API without hostPID
- [ ] Intel Gaudi / Habana support via `hl-smi`
- [ ] Ollama token estimation via response body interception
- [ ] Pre-built binaries and container images
- [ ] Helm chart for Kubernetes deployment
