# Changelog

All notable changes to Inferscope will be documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Inferscope uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [0.2.0] — 2026-05-07

### Fixed

**Process discovery**
- Port scanner rewritten to use socket inode cross-referencing instead of
  reading the shared `/proc/<pid>/net/tcp` table. The old approach returned
  every port listening on the host for any host-network process (including
  display servers and the Axon binary itself), causing all of them to scrape
  inference endpoints they did not own. The new implementation reads
  `/proc/<pid>/fd/` to collect socket inodes for a process and only accepts
  TCP LISTEN entries whose inode appears in that set — correct across network
  namespaces and essential for Kubernetes deployments where many services share
  the host network.
- Added `isNonInferenceComm()` filter in the process scanner to skip
  well-known non-inference processes that open GPU device files for
  non-inference reasons: `Xorg`, `Xwayland`, `Xvfb`, and `axon` itself.

**Token metrics**
- Token metrics were being attributed to every process that happened to
  discover the same metrics endpoint URL (a side effect of the port scanner
  bug above). Added endpoint-URL deduplication in the token collector: after
  all processes are scraped, only the process with the most specific runtime
  (`nim`/`vllm`/`llama.cpp`/`tgi`/`ollama` > `unknown`) retains the token
  metrics for a given endpoint. Remaining processes are downgraded to
  `source="none"`.
- Fixed duplicate emission of `gpu_process_running` metric: it was emitted
  once from the NVML `ProcessMemory` loop and again from the token collector
  loop with identical label values, causing a Prometheus collection error that
  made the entire `/metrics` endpoint return an error response.

**NVIDIA power reporting**
- Added `GetTotalEnergyConsumption()` fallback for GPUs that do not support
  `GetPowerUsage()` (e.g. RTX A1000 mobile). Power is computed as
  `ΔmJ / Δms` between successive poll cycles. Note: the RTX A1000 does not
  support energy consumption reporting either — `gpu_power_watts` will remain
  0 on that hardware regardless of method.

**Grafana dashboards**
- Fixed Grafana 11 compatibility: template variable `query` fields were plain
  strings. Grafana 11 requires the object form
  `{"query": "...", "refId": "StandardVariableQuery"}`. Fixed across all five
  dashboard JSON files.
- Fixed VRAM percentage queries that produced `400 bad_data` errors:
  `avg_over_time((A / B)[$range])` is invalid PromQL (ranges cannot be applied
  to binary expressions). Rewritten as
  `avg_over_time(A[$range]) / avg_over_time(B[$range])`.
- Fixed Utilization Distribution chart showing no data when GPU utilization
  was below 75%: the comparison operator `>= 75` filters (drops) series that
  don't match, leaving nothing to average. Changed to `>= bool 75` / `< bool 75`
  to return 0 or 1 for every series regardless of value.
- Added VRAM panels to the Overview dashboard: "Avg VRAM Used" (%) and
  "VRAM Used (MiB)". Existing six stat panels resized from `w=4` to `w=3` to
  accommodate two new panels in the same row.
- Fixed Token Consumption breakdown table: `gpu_name` is not a label on token
  metrics but was included in `sum by (...)`, producing inconsistent data frame
  shapes between the four time-window queries and breaking the `joinByField`
  table transformation. Removed from all aggregation clauses.

**Prometheus provisioning (single-host stack)**
- Grafana datasource provisioning was missing `uid: prometheus`, causing
  Grafana to assign a random UID on first start. All five dashboards reference
  `uid: prometheus` — the mismatch caused every panel to silently return no
  data. Fixed by pinning `uid: prometheus` in
  `deploy/single-host/grafana/provisioning/datasources/prometheus.yml`.
- Prometheus scrape config had a `relabel_configs` rule that extracted the
  node label from `__address__` (resolving to `localhost`), overwriting
  Axon's own `node` label with the correct hostname. Replaced with a simple
  `cluster: local` injection only.

---

## [0.1.0] — 2026-05-06

This is the first version of Inferscope. It establishes the core architecture
and all three GPU vendor collectors.

### Added

**Axon — node agent**
- `/proc` fd scanning to auto-discover GPU-using inference processes without
  any prior configuration or runtime modification
- Sysfs PCI vendor ID resolution (`/sys/class/drm/renderD<N>/device/vendor`)
  to correctly distinguish AMD and Intel render nodes on mixed-vendor systems
- Runtime detection from process cmdline and environment (vLLM, llama.cpp,
  NIM, TGI, Ollama, unknown)
- Model name extraction from cmdline flags (`--model`, `-m`, etc.) and
  environment variables (`OLLAMA_MODEL`, `HF_MODEL_ID`, `NIM_MODEL_NAME`)
- Listening port discovery via `/proc/<pid>/net/tcp` cross-referenced against
  `/proc/<pid>/fd/` socket inodes — no `ss` or `netstat` required

**GPU hardware collectors**
- AMD collector via `rocm-smi --json` (ROCm 5.x / 6.x)
- NVIDIA collector via NVML / `go-nvml` bindings — includes per-process VRAM
  breakdown from `GetComputeRunningProcesses()`
- Intel Arc / Xe collector via `intel_gpu_top -J` and sysfs hwmon — covers
  render engine utilization, LMEM/GTT memory, power, temperature, and clock
- Shared `common.DeviceMetrics` struct and `common.HardwareCollector` interface
  so the exporter handles all three vendors identically with no per-vendor
  type assertions — adding a future vendor requires only a new collector package

**Token metrics collector**
- Opportunistic `/metrics` scraping on every port a discovered process listens on
- Tries `/metrics`, `/v1/metrics` (NIM), and `/api/metrics` in order
- Named metric mappings for vLLM (`vllm:` prefix), llama.cpp (`llamacpp:`
  prefix), TGI, and generic fallback patterns
- Ollama detection via `/api/ps` — marks source as `ollama_api` and exports
  hardware metrics only (Ollama has no Prometheus token endpoint)
- Raw passthrough of all token-related metrics from the runtime endpoint

**Prometheus exporter**
- Hardware metrics: `gpu_utilization_percent`, `gpu_memory_used_mib`,
  `gpu_memory_total_mib`, `gpu_power_watts`, `gpu_temperature_celsius`,
  `gpu_clock_mhz`
- Process metrics: `gpu_process_running`, `gpu_process_memory_mib`
- Token metrics: `gpu_inference_prompt_tokens_total`,
  `gpu_inference_generated_tokens_total`, `gpu_inference_tokens_per_second`,
  `gpu_inference_tokens_per_watt`, `gpu_inference_raw_metric`,
  `gpu_inference_token_source`
- Labels on all metrics: `gpu_id`, `vendor`, `gpu_name`, `node`
- Efficiency metric `gpu_inference_tokens_per_watt` computed automatically
  from token throughput and GPU power draw

**Deployment**
- systemd service unit (`deploy/systemd/axon.service`)
- Axon-only Docker Compose (`deploy/docker/`)
- Single-host Docker Compose — Axon + Prometheus + Grafana all-in-one (`deploy/single-host/`)
- Kubernetes DaemonSet with `hostPID: true` and `hostNetwork: true` (`deploy/kubernetes/daemonset.yaml`)
- Kubernetes RBAC — ServiceAccount, ClusterRole, ClusterRoleBinding (`deploy/kubernetes/rbac.yaml`)
- Kubernetes ServiceMonitor for Prometheus Operator (`deploy/kubernetes/service-monitor.yaml`)
- vmagent Kubernetes scrape config with pod service discovery (`deploy/kubernetes/vmagent-scrape.yml`)
- Local development stack: Prometheus + Grafana (`deploy/local/`)
- Makefile targets: `build`, `systemd-install`, `systemd-start`, `systemd-stop`,
  `systemd-logs`, `local-up`, `local-down`, `local-purge`, `uninstall`, `purge-all`, `docker-build`

**Dashboards**

Five Grafana dashboards in `dashboards/`, all auto-provisioned in the
single-host and local deployment stacks. All share cascading
Cluster → Node → GPU dropdown variables.

- `dashboards/overview.json` — cross-cluster summary: active nodes/GPUs/models,
  token throughput time series, 24h token bar chart per node, GPU utilization,
  tokens per watt, active inference processes table
- `dashboards/tokens.json` — token consumption detail: fixed 1h/6h/12h/24h
  comparison table with totals footer, trend charts per node and per GPU,
  per-model and per-node donut charts, running models table
- `dashboards/gpu-hardware.json` — raw hardware metrics: GPU inventory table
  with color-coded utilization/VRAM/power/temperature, time series for each
  metric, 24h average utilization and power bar gauges
- `dashboards/power-efficiency.json` — energy and efficiency: tokens/watt
  ranking, power draw ranking, estimated kWh, tokens per kWh, power and
  efficiency time series, per-node summary table with average power/tok/s/tok/W
- `dashboards/utilization.json` — hardware ROI view: fleet average utilization
  gauge vs 75% target, GPUs above/below target counters, VRAM gauge, per-GPU
  utilization and VRAM bar charts, utilization time series with 75% target line,
  VRAM over time, utilization distribution chart, detailed per-GPU table with
  "target met" ✓/✗ column for management reporting

Grafana provisioning configuration included in `deploy/single-host/` and
`deploy/local/` — dashboards load automatically on first startup.

**Flags**
- `--listen-address` (default `:9101`)
- `--poll-interval` (default `5s`)
- `--hw-interval` (default `2s`)
- `--amd` / `--nvidia` / `--intel` (all default `true`)

[0.1.0]: https://github.com/youorg/inferscope/releases/tag/v0.1.0
