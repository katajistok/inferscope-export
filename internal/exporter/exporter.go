// Package exporter assembles metrics from all collectors into a Prometheus
// exposition. It depends only on common.HardwareCollector and tokens.Collector,
// so adding a new GPU vendor requires zero changes here.
package exporter

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/youorg/inferscope/internal/collector/common"
	"github.com/youorg/inferscope/internal/collector/tokens"
	"github.com/youorg/inferscope/internal/discovery"
)

var hostname string

func init() {
	h, err := os.Hostname()
	if err != nil {
		h = "unknown"
	}
	hostname = h
}

// Exporter is the top-level Prometheus collector for Inferscope/Axon.
type Exporter struct {
	disc        *discovery.Discoverer
	hwCollectors []common.HardwareCollector // AMD, NVIDIA, Intel — all treated identically
	tokenCol    *tokens.Collector

	// GPU hardware descriptors
	gpuUtil    *prometheus.Desc
	gpuMemUsed *prometheus.Desc
	gpuMemTotal *prometheus.Desc
	gpuPower   *prometheus.Desc
	gpuTemp    *prometheus.Desc
	gpuClock   *prometheus.Desc

	// Process / workload descriptors
	procMemUsed *prometheus.Desc
	procRunning *prometheus.Desc

	// Token descriptors
	tokenPrompt    *prometheus.Desc
	tokenGenerated *prometheus.Desc
	tokenPerSec    *prometheus.Desc
	tokenRaw       *prometheus.Desc
	tokenSource    *prometheus.Desc
	tokenPerWatt   *prometheus.Desc
}

// New creates the Prometheus exporter. hwCollectors may contain any mix of
// AMD, NVIDIA, and Intel collectors — they are all handled identically via
// the common.HardwareCollector interface.
func New(disc *discovery.Discoverer, hwCollectors []common.HardwareCollector, tokenCol *tokens.Collector) *Exporter {
	hw := []string{"gpu_id", "vendor", "gpu_name", "node"}
	proc := []string{"gpu_id", "vendor", "gpu_name", "node", "pid", "model", "runtime"}
	tok := []string{"gpu_id", "vendor", "node", "pid", "model", "runtime", "source"}

	return &Exporter{
		disc:         disc,
		hwCollectors: hwCollectors,
		tokenCol:     tokenCol,

		gpuUtil: prometheus.NewDesc(
			"gpu_utilization_percent",
			"GPU compute/render engine utilization percentage",
			hw, nil),
		gpuMemUsed: prometheus.NewDesc(
			"gpu_memory_used_mib",
			"GPU VRAM used in MiB",
			hw, nil),
		gpuMemTotal: prometheus.NewDesc(
			"gpu_memory_total_mib",
			"GPU VRAM total in MiB",
			hw, nil),
		gpuPower: prometheus.NewDesc(
			"gpu_power_watts",
			"GPU power draw in watts",
			hw, nil),
		gpuTemp: prometheus.NewDesc(
			"gpu_temperature_celsius",
			"GPU core temperature in Celsius",
			hw, nil),
		gpuClock: prometheus.NewDesc(
			"gpu_clock_mhz",
			"GPU core clock in MHz",
			hw, nil),

		procMemUsed: prometheus.NewDesc(
			"gpu_process_memory_mib",
			"VRAM used by a specific process in MiB (NVIDIA only via NVML)",
			proc, nil),
		procRunning: prometheus.NewDesc(
			"gpu_process_running",
			"1 if an inference process is currently active on this GPU",
			proc, nil),

		tokenPrompt: prometheus.NewDesc(
			"gpu_inference_prompt_tokens_total",
			"Total prompt (input) tokens processed by this inference process",
			tok, nil),
		tokenGenerated: prometheus.NewDesc(
			"gpu_inference_generated_tokens_total",
			"Total tokens generated (output) by this inference process",
			tok, nil),
		tokenPerSec: prometheus.NewDesc(
			"gpu_inference_tokens_per_second",
			"Current token generation throughput in tokens/s",
			tok, nil),
		tokenRaw: prometheus.NewDesc(
			"gpu_inference_raw_metric",
			"Raw token-related metric scraped verbatim from the runtime /metrics endpoint",
			append(tok, "metric_name"), nil),
		tokenSource: prometheus.NewDesc(
			"gpu_inference_token_source",
			"1 with a label indicating how token metrics were collected",
			append(tok, "scrape_source"), nil),
		tokenPerWatt: prometheus.NewDesc(
			"gpu_inference_tokens_per_watt",
			"Token generation efficiency: tokens/s divided by GPU power in watts",
			tok, nil),
	}
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range e.allDescs() {
		ch <- d
	}
}

func (e *Exporter) allDescs() []*prometheus.Desc {
	return []*prometheus.Desc{
		e.gpuUtil, e.gpuMemUsed, e.gpuMemTotal, e.gpuPower, e.gpuTemp, e.gpuClock,
		e.procMemUsed, e.procRunning,
		e.tokenPrompt, e.tokenGenerated, e.tokenPerSec,
		e.tokenRaw, e.tokenSource, e.tokenPerWatt,
	}
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	// powerByGPUID is used to compute the tokens-per-watt efficiency metric.
	// Key is "<vendor>/<gpu_id>" to avoid collisions on mixed-vendor nodes.
	powerByGPUID := make(map[string]float64)

	// ── Hardware metrics ─────────────────────────────────────────────────
	// All vendors go through the same loop — no per-vendor type assertions.
	for _, col := range e.hwCollectors {
		for _, d := range col.GetDevices() {
			id := strconv.Itoa(d.Index)
			hwLabels := []string{id, d.Vendor, d.Name, hostname}
			powerKey := d.Vendor + "/" + id
			powerByGPUID[powerKey] = d.PowerWatts

			e.gauge(ch, e.gpuUtil,    d.UtilPercent, hwLabels...)
			e.gauge(ch, e.gpuMemUsed, d.MemUsedMiB,  hwLabels...)
			e.gauge(ch, e.gpuMemTotal,d.MemTotalMiB, hwLabels...)
			e.gauge(ch, e.gpuPower,   d.PowerWatts,  hwLabels...)
			e.gauge(ch, e.gpuTemp,    d.TempCelsius, hwLabels...)
			e.gauge(ch, e.gpuClock,   d.ClockMHz,    hwLabels...)

			// Per-process VRAM — only NVIDIA populates ProcessMemory via NVML.
			// AMD and Intel return an empty map; the loop simply does nothing.
			for pid, memBytes := range d.ProcessMemory {
				proc := e.findProcess(pid)
				model, runtime := resolveProcess(proc)
				procLabels := []string{id, d.Vendor, d.Name, hostname,
					strconv.Itoa(pid), model, runtime}
				e.gauge(ch, e.procMemUsed, float64(memBytes)/(1024*1024), procLabels...)
				e.gauge(ch, e.procRunning, 1, procLabels...)
			}
		}
	}

	// ── Token metrics ─────────────────────────────────────────────────────
	for _, m := range e.tokenCol.GetMetrics() {
		gpuID := gpuIDsStr(m.GPUIDs)
		tokLabels := []string{gpuID, string(m.Vendor), hostname,
			strconv.Itoa(m.PID), m.ModelName, string(m.Runtime), m.Source}

		if m.Source == "none" {
			continue
		}

		e.counter(ch, e.tokenPrompt,    m.PromptTokensTotal,   tokLabels...)
		e.counter(ch, e.tokenGenerated, m.GeneratedTokensTotal, tokLabels...)
		e.gauge(ch,   e.tokenPerSec,    m.TokensPerSecond,      tokLabels...)
		e.gauge(ch,   e.tokenSource,    1, append(tokLabels, m.Source)...)

		// Efficiency metric — only meaningful when both values are non-zero
		powerKey := string(m.Vendor) + "/" + gpuID
		if m.TokensPerSecond > 0 && powerByGPUID[powerKey] > 0 {
			e.gauge(ch, e.tokenPerWatt,
				m.TokensPerSecond/powerByGPUID[powerKey], tokLabels...)
		}

		// Pass through all raw token-related metrics scraped from /metrics.
		// Always emitted as Gauge — the metric_name label preserves the original
		// name (including _total suffix for counters) for PromQL disambiguation.
		for name, val := range m.RawGauges {
			e.gauge(ch, e.tokenRaw, val, append(tokLabels, sanitize(name))...)
		}
		for name, val := range m.RawCounters {
			e.gauge(ch, e.tokenRaw, val, append(tokLabels, sanitize(name))...)
		}
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

func (e *Exporter) gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, val float64, labels ...string) {
	m, err := prometheus.NewConstMetric(desc, prometheus.GaugeValue, val, labels...)
	if err != nil {
		log.Printf("exporter: gauge error (%s): %v", desc, err)
		return
	}
	ch <- m
}

func (e *Exporter) counter(ch chan<- prometheus.Metric, desc *prometheus.Desc, val float64, labels ...string) {
	m, err := prometheus.NewConstMetric(desc, prometheus.CounterValue, val, labels...)
	if err != nil {
		log.Printf("exporter: counter error (%s): %v", desc, err)
		return
	}
	ch <- m
}

func (e *Exporter) findProcess(pid int) *discovery.GPUProcess {
	for _, p := range e.disc.GetProcesses() {
		if p.PID == pid {
			return p
		}
	}
	return nil
}

// gpuNameFor looks up the GPU name from the hardware collectors by vendor and index.
func (e *Exporter) gpuNameFor(vendor string, idx int) string {
	for _, col := range e.hwCollectors {
		if col.Name() != vendor {
			continue
		}
		for _, d := range col.GetDevices() {
			if d.Index == idx {
				return d.Name
			}
		}
	}
	return fmt.Sprintf("%s-gpu-%d", vendor, idx)
}

func resolveProcess(proc *discovery.GPUProcess) (model, runtime string) {
	if proc != nil {
		return proc.ModelName, string(proc.Runtime)
	}
	return "unknown", "unknown"
}

func gpuIDsStr(ids []int) string {
	if len(ids) == 0 {
		return "unknown"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ",")
}

// sanitize makes a string safe for use as a Prometheus label value.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == ':' {
			return r
		}
		return '_'
	}, s)
}
