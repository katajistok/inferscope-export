// Package intel collects hardware metrics from Intel GPUs (Arc, Iris Xe, Data Center GPU Max)
// via intel_gpu_top (part of igt-gpu-tools) and Linux sysfs.
//
// Requirements:
//   - intel_gpu_top installed: apt install intel-gpu-tools
//   - Running as root or with CAP_PERFMON (kernel ≥ 5.8) for perf counters
//   - /sys/class/drm/card* readable for VRAM and power info
//
// intel_gpu_top -J outputs a stream of JSON samples; we request a single
// sample with -s 1 (1 ms interval) and -c 1 (1 sample count) and parse it.
package intel

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/youorg/inferscope/internal/collector/common"
)

// Collector polls Intel GPU metrics and implements common.HardwareCollector.
type Collector struct {
	mu       sync.RWMutex
	devices  []common.DeviceMetrics
	interval time.Duration
	stop     chan struct{}
}

// NewCollector discovers Intel GPUs via sysfs and starts background polling.
// Returns an error if no Intel GPUs are found or intel_gpu_top is unavailable.
func NewCollector(interval time.Duration) (*Collector, error) {
	if _, err := exec.LookPath("intel_gpu_top"); err != nil {
		return nil, fmt.Errorf("intel_gpu_top not found in PATH (install intel-gpu-tools): %w", err)
	}
	gpus := discoverIntelGPUs()
	if len(gpus) == 0 {
		return nil, fmt.Errorf("no Intel GPUs found in /sys/class/drm")
	}
	log.Printf("intel: discovered %d GPU(s): %v", len(gpus), gpus)

	c := &Collector{
		interval: interval,
		stop:     make(chan struct{}),
	}
	if err := c.poll(); err != nil {
		// Non-fatal on first poll — perf counters may need a moment
		log.Printf("intel: initial poll warning: %v", err)
	}
	go c.loop()
	return c, nil
}

func (c *Collector) Name() string { return "intel" }

func (c *Collector) GetDevices() []common.DeviceMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]common.DeviceMetrics, len(c.devices))
	copy(out, c.devices)
	return out
}

func (c *Collector) Stop() { close(c.stop) }

func (c *Collector) loop() {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := c.poll(); err != nil {
				log.Printf("intel: poll error: %v", err)
			}
		case <-c.stop:
			return
		}
	}
}

// intelGPUTopOutput is the JSON structure from intel_gpu_top -J.
// Only the fields we use are mapped; unknown fields are ignored.
type intelGPUTopOutput struct {
	Period struct {
		Duration float64 `json:"duration"`
	} `json:"period"`
	Engines map[string]struct {
		Busy float64 `json:"busy"` // percent
	} `json:"engines"`
	Memory struct {
		// intel_gpu_top reports memory in bytes
		Actual struct {
			Total float64 `json:"total"`
			Used  float64 `json:"used"`
		} `json:"actual"`
		LMEM struct {
			Total float64 `json:"total"`
			Used  float64 `json:"used"`
		} `json:"lmem"` // Local memory (discrete GPUs)
	} `json:"memory"`
	Power struct {
		GPU     float64 `json:"gpu"`     // watts
		Package float64 `json:"package"` // watts (fallback)
	} `json:"power"`
	Frequency struct {
		Requested float64 `json:"requested"` // MHz
		Actual    float64 `json:"actual"`    // MHz
	} `json:"frequency"`
}

func (c *Collector) poll() error {
	// -J  → JSON output
	// -s 100 → sample interval 100ms (minimum reliable value)
	// -c 1   → collect 1 sample then exit
	out, err := exec.Command("intel_gpu_top", "-J", "-s", "100", "-c", "1").Output()
	if err != nil {
		return fmt.Errorf("intel_gpu_top failed: %w", err)
	}

	// intel_gpu_top outputs one JSON object per line; take the last non-empty line
	// (first line is often incomplete or a header)
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var sample intelGPUTopOutput
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || line == "[" || line == "]" {
			continue
		}
		// Strip leading comma if present (JSON array element)
		line = strings.TrimPrefix(line, ",")
		if err := json.Unmarshal([]byte(line), &sample); err == nil {
			break
		}
	}

	// Collect sysfs metrics (temp, additional info) per card
	sysfsDevices := discoverIntelGPUs()

	var devices []common.DeviceMetrics
	for idx, cardName := range sysfsDevices {
		d := common.DeviceMetrics{
			Index:         idx,
			Vendor:        "intel",
			Name:          readGPUName(cardName),
			ProcessMemory: make(map[int]uint64),
		}

		// Render engine utilization — prefer "Render/3D" engine
		for name, eng := range sample.Engines {
			lower := strings.ToLower(name)
			if strings.Contains(lower, "render") || strings.Contains(lower, "3d") {
				d.UtilPercent = eng.Busy
				break
			}
		}

		// Memory: prefer LMEM (discrete VRAM), fall back to system memory estimate
		if sample.Memory.LMEM.Total > 0 {
			d.MemTotalMiB = sample.Memory.LMEM.Total / (1024 * 1024)
			d.MemUsedMiB = sample.Memory.LMEM.Used / (1024 * 1024)
		} else if sample.Memory.Actual.Total > 0 {
			d.MemTotalMiB = sample.Memory.Actual.Total / (1024 * 1024)
			d.MemUsedMiB = sample.Memory.Actual.Used / (1024 * 1024)
		}
		// Fallback: read from sysfs if intel_gpu_top gave nothing
		if d.MemTotalMiB == 0 {
			d.MemTotalMiB, d.MemUsedMiB = readSysfsMemory(cardName)
		}

		// Power
		d.PowerWatts = sample.Power.GPU
		if d.PowerWatts == 0 {
			d.PowerWatts = sample.Power.Package
		}
		if d.PowerWatts == 0 {
			d.PowerWatts = readSysfsPower(cardName)
		}

		// Frequency
		d.ClockMHz = sample.Frequency.Actual
		if d.ClockMHz == 0 {
			d.ClockMHz = sample.Frequency.Requested
		}

		// Temperature via sysfs hwmon
		d.TempCelsius = readSysfsTemp(cardName)

		devices = append(devices, d)
	}

	// If intel_gpu_top gave no usable output, build minimal entries from sysfs only
	if len(devices) == 0 {
		for idx, cardName := range sysfsDevices {
			total, used := readSysfsMemory(cardName)
			devices = append(devices, common.DeviceMetrics{
				Index:         idx,
				Vendor:        "intel",
				Name:          readGPUName(cardName),
				MemTotalMiB:   total,
				MemUsedMiB:    used,
				PowerWatts:    readSysfsPower(cardName),
				TempCelsius:   readSysfsTemp(cardName),
				ProcessMemory: make(map[int]uint64),
			})
		}
	}

	c.mu.Lock()
	c.devices = devices
	c.mu.Unlock()
	return nil
}

// ── sysfs helpers ──────────────────────────────────────────────────────────

// discoverIntelGPUs returns card names (e.g. "card0") for all Intel GPUs
// identified by PCI vendor ID 0x8086 in /sys/class/drm.
func discoverIntelGPUs() []string {
	entries, err := filepath.Glob("/sys/class/drm/card*")
	if err != nil {
		return nil
	}
	var cards []string
	for _, entry := range entries {
		// Skip render nodes and other non-card entries
		base := filepath.Base(entry)
		if strings.Contains(base, "-") {
			continue // e.g. card0-HDMI-1
		}
		vendorPath := filepath.Join(entry, "device", "vendor")
		data, err := os.ReadFile(vendorPath)
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(data)) == "0x8086" {
			cards = append(cards, base)
		}
	}
	return cards
}

// readGPUName reads the GPU product name from sysfs.
func readGPUName(cardName string) string {
	// Try PCI subsystem name first (usually more descriptive)
	for _, p := range []string{
		fmt.Sprintf("/sys/class/drm/%s/device/subsystem_device", cardName),
		fmt.Sprintf("/sys/class/drm/%s/device/label", cardName),
	} {
		if data, err := os.ReadFile(p); err == nil {
			name := strings.TrimSpace(string(data))
			if name != "" && name != "0x0000" {
				return name
			}
		}
	}
	// Fall back to the uevent for a device description
	data, err := os.ReadFile(fmt.Sprintf("/sys/class/drm/%s/device/uevent", cardName))
	if err != nil {
		return fmt.Sprintf("Intel GPU (%s)", cardName)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "PCI_ID=") {
			return fmt.Sprintf("Intel GPU %s", strings.TrimPrefix(line, "PCI_ID="))
		}
	}
	return fmt.Sprintf("Intel GPU (%s)", cardName)
}

// readSysfsMemory returns (totalMiB, usedMiB) from sysfs GTT/VRAM files.
// These are available on discrete Intel GPUs (Arc, Data Center GPU Max).
func readSysfsMemory(cardName string) (total, used float64) {
	// GTT = Graphics Translation Table memory (shared + dedicated)
	// mem_info_vram_total / mem_info_vram_used — discrete VRAM (Arc, Xe)
	for _, pair := range []struct{ totalPath, usedPath string }{
		{
			fmt.Sprintf("/sys/class/drm/%s/device/mem_info_vram_total", cardName),
			fmt.Sprintf("/sys/class/drm/%s/device/mem_info_vram_used", cardName),
		},
		{
			fmt.Sprintf("/sys/class/drm/%s/device/mem_info_gtt_total", cardName),
			fmt.Sprintf("/sys/class/drm/%s/device/mem_info_gtt_used", cardName),
		},
	} {
		t := readSysfsBytes(pair.totalPath)
		u := readSysfsBytes(pair.usedPath)
		if t > 0 {
			return t / (1024 * 1024), u / (1024 * 1024)
		}
	}
	return 0, 0
}

// readSysfsPower returns GPU power in watts from the hwmon subsystem.
func readSysfsPower(cardName string) float64 {
	hwmonBase := fmt.Sprintf("/sys/class/drm/%s/device/hwmon", cardName)
	entries, err := filepath.Glob(hwmonBase + "/hwmon*")
	if err != nil || len(entries) == 0 {
		return 0
	}
	for _, hwmon := range entries {
		// power1_input is in microwatts
		for _, f := range []string{"power1_input", "power1_average"} {
			if v := readSysfsFloat(filepath.Join(hwmon, f)); v > 0 {
				return v / 1_000_000 // µW → W
			}
		}
	}
	return 0
}

// readSysfsTemp returns GPU temperature in Celsius from hwmon.
func readSysfsTemp(cardName string) float64 {
	hwmonBase := fmt.Sprintf("/sys/class/drm/%s/device/hwmon", cardName)
	entries, err := filepath.Glob(hwmonBase + "/hwmon*")
	if err != nil || len(entries) == 0 {
		return 0
	}
	for _, hwmon := range entries {
		// temp1_input is in millidegrees Celsius
		if v := readSysfsFloat(filepath.Join(hwmon, "temp1_input")); v > 0 {
			return v / 1000 // m°C → °C
		}
	}
	return 0
}

func readSysfsFloat(path string) float64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	f, _ := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	return f
}

func readSysfsBytes(path string) float64 {
	return readSysfsFloat(path)
}
