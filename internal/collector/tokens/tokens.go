// Package tokens discovers inference processes and scrapes token metrics from
// their Prometheus /metrics endpoints. It is entirely runtime-agnostic —
// it tries each listening port with common metrics paths and parses whatever
// Prometheus-format response it finds.
package tokens

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/youorg/inferscope/internal/discovery"
)

// TokenMetrics holds token accounting for one inference process.
type TokenMetrics struct {
	PID       int
	ModelName string
	Runtime   discovery.Runtime
	Vendor    discovery.Vendor
	GPUIDs    []int
	MetricsPort int
	MetricsPath string

	// Token counters — zero if source has none
	PromptTokensTotal    float64
	GeneratedTokensTotal float64
	TokensPerSecond      float64

	// Raw scraped metrics for passthrough
	RawGauges   map[string]float64
	RawCounters map[string]float64

	Source string // "prometheus", "ollama_api", or "none"
}

// knownMetricsPaths are tried in order for each listening port.
var knownMetricsPaths = []string{
	"/metrics",
	"/v1/metrics", // NVIDIA NIM
	"/api/metrics",
}

// tokenMetricPatterns identify token-related metrics by name substring.
var tokenMetricPatterns = []string{
	"token", "prompt", "generated", "inference", "request",
}

// Collector continuously discovers and scrapes token metrics from GPU processes.
type Collector struct {
	disc     *discovery.Discoverer
	mu       sync.RWMutex
	metrics  map[int]*TokenMetrics
	client   *http.Client
	interval time.Duration
	stop     chan struct{}
}

// NewCollector creates and starts a token metrics collector.
func NewCollector(disc *discovery.Discoverer, interval time.Duration) *Collector {
	c := &Collector{
		disc:     disc,
		metrics:  make(map[int]*TokenMetrics),
		interval: interval,
		stop:     make(chan struct{}),
		client:   &http.Client{Timeout: 2 * time.Second},
	}
	go c.loop()
	return c
}

func (c *Collector) Name() string { return "tokens" }

// GetMetrics returns a snapshot of token metrics for all discovered processes.
func (c *Collector) GetMetrics() []*TokenMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*TokenMetrics, 0, len(c.metrics))
	for _, m := range c.metrics {
		out = append(out, m)
	}
	return out
}

func (c *Collector) Stop() { close(c.stop) }

func (c *Collector) loop() {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			c.collect()
		case <-c.stop:
			return
		}
	}
}

func (c *Collector) collect() {
	procs := c.disc.GetProcesses()
	result := make(map[int]*TokenMetrics, len(procs))

	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, proc := range procs {
		proc := proc
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := c.scrapeProcess(proc)
			mu.Lock()
			result[proc.PID] = m
			mu.Unlock()
		}()
	}
	wg.Wait()

	// Deduplicate: if multiple processes scraped the same endpoint, keep only
	// the one with the most specific runtime (nim/vllm/llama.cpp > unknown).
	claimed := make(map[string]int) // "host:port/path" → pid that owns it
	for pid, m := range result {
		if m.Source != "prometheus" {
			continue
		}
		key := fmt.Sprintf("127.0.0.1:%d%s", m.MetricsPort, m.MetricsPath)
		if ownerPID, exists := claimed[key]; exists {
			ownerRuntime := result[ownerPID].Runtime
			if runtimePriority(m.Runtime) > runtimePriority(ownerRuntime) {
				result[ownerPID].Source = "none"
				claimed[key] = pid
			} else {
				m.Source = "none"
			}
		} else {
			claimed[key] = pid
		}
	}

	c.mu.Lock()
	c.metrics = result
	c.mu.Unlock()
}

func runtimePriority(r discovery.Runtime) int {
	switch r {
	case discovery.RuntimeNIM, discovery.RuntimeVLLM, discovery.RuntimeLlamaCpp, discovery.RuntimeTGI, discovery.RuntimeOllama:
		return 1
	default:
		return 0
	}
}

func (c *Collector) scrapeProcess(proc *discovery.GPUProcess) *TokenMetrics {
	m := &TokenMetrics{
		PID:       proc.PID,
		ModelName: proc.ModelName,
		Runtime:   proc.Runtime,
		Vendor:    proc.Vendor,
		GPUIDs:    proc.GPUIDs,
		Source:    "none",
	}

	for _, port := range proc.ListenPorts {
		for _, path := range knownMetricsPaths {
			url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
			if c.tryPrometheus(url, m) {
				m.MetricsPort = port
				m.MetricsPath = path
				m.Source = "prometheus"
				log.Printf("tokens: pid=%d model=%s → scraped %s", proc.PID, proc.ModelName, url)
				return m
			}
		}
	}

	// Ollama fallback — marks source but provides no token counts
	for _, port := range proc.ListenPorts {
		if c.tryOllamaAPI(port) {
			m.MetricsPort = port
			m.Source = "ollama_api"
			return m
		}
	}

	return m
}

// tryPrometheus fetches and parses a Prometheus text exposition endpoint.
// Returns true if a valid response containing token-related metrics was found.
func (c *Collector) tryPrometheus(url string, m *TokenMetrics) bool {
	resp, err := c.client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/") && !strings.Contains(ct, "openmetrics") {
		return false
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false
	}
	lines := strings.Split(string(body), "\n")
	if len(lines) < 3 {
		return false
	}

	m.RawGauges = make(map[string]float64)
	m.RawCounters = make(map[string]float64)
	foundToken := false

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := parseMetricLine(line)
		if !ok {
			continue
		}
		lower := strings.ToLower(name)

		for _, pat := range tokenMetricPatterns {
			if strings.Contains(lower, pat) {
				foundToken = true
				break
			}
		}

		// ── vLLM (prefix: vllm:) ─────────────────────────────────────────
		switch {
		case matchAny(lower,
			"vllm:prompt_tokens_total", "vllm:num_prompt_tokens_total",
			"prompt_tokens_total", "num_prompt_tokens_total"):
			m.PromptTokensTotal = value

		case matchAny(lower,
			"vllm:generation_tokens_total", "vllm:num_generation_tokens_total",
			"generation_tokens_total", "num_generation_tokens_total",
			"generated_tokens_total", "output_tokens_total"):
			m.GeneratedTokensTotal = value

		case matchAny(lower,
			"vllm:avg_generation_throughput_toks_per_s",
			"avg_generation_throughput_toks_per_s",
			"generation_tokens_per_second"):
			m.TokensPerSecond = value

		// ── llama.cpp server (prefix: llamacpp:) ─────────────────────────
		// Requires --metrics flag at server startup.
		case matchAny(lower,
			"llamacpp:tokens_per_second",
			"llamacpp:eval_tokens_per_second"):
			m.TokensPerSecond = value

		case matchAny(lower,
			"llamacpp:prompt_tokens_total",
			"llamacpp:tokens_evaluated_total"):
			m.PromptTokensTotal = value

		case matchAny(lower,
			"llamacpp:tokens_generated_total",
			"llamacpp:tokens_decoded_total",
			"llamacpp:generation_tokens_total"):
			m.GeneratedTokensTotal = value

		// ── TGI (Hugging Face Text Generation Inference) ──────────────────
		case matchAny(lower, "tgi_batch_input_tokens_total"):
			m.PromptTokensTotal = value
		case matchAny(lower,
			"tgi_request_generated_tokens_total",
			"tgi_batch_generated_tokens_total"):
			m.GeneratedTokensTotal = value

		// ── Generic fallback patterns ─────────────────────────────────────
		case strings.Contains(lower, "prompt_tokens") && strings.HasSuffix(lower, "_total"):
			if m.PromptTokensTotal == 0 {
				m.PromptTokensTotal = value
			}
		case strings.Contains(lower, "generated_tokens") && strings.HasSuffix(lower, "_total"):
			if m.GeneratedTokensTotal == 0 {
				m.GeneratedTokensTotal = value
			}
		case strings.Contains(lower, "tokens_per_second") || strings.Contains(lower, "tokens_second"):
			if m.TokensPerSecond == 0 {
				m.TokensPerSecond = value
			}
		}

		// Store all token-related metrics as raw passthrough
		for _, pat := range tokenMetricPatterns {
			if strings.Contains(lower, pat) {
				if strings.HasSuffix(lower, "_total") || strings.Contains(lower, "count") {
					m.RawCounters[name] = value
				} else {
					m.RawGauges[name] = value
				}
				break
			}
		}
	}

	return foundToken
}

// tryOllamaAPI checks if a port serves the Ollama REST API.
// Ollama has no token counters — we just mark the source so the dashboard
// can indicate that hardware metrics are available but token counts are not.
func (c *Collector) tryOllamaAPI(port int) bool {
	resp, err := c.client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ps", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK &&
		strings.Contains(resp.Header.Get("Content-Type"), "application/json")
}

// parseMetricLine parses one line of Prometheus text format into name + value.
func parseMetricLine(line string) (name string, value float64, ok bool) {
	scanner := bufio.NewScanner(strings.NewReader(line))
	scanner.Split(bufio.ScanWords)
	if !scanner.Scan() {
		return
	}
	nameWithLabels := scanner.Text()
	if !scanner.Scan() {
		return
	}
	valueStr := scanner.Text()

	if idx := strings.Index(nameWithLabels, "{"); idx >= 0 {
		name = nameWithLabels[:idx]
	} else {
		name = nameWithLabels
	}
	fmt.Sscanf(valueStr, "%f", &value)
	ok = true
	return
}

func matchAny(s string, patterns ...string) bool {
	for _, p := range patterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
