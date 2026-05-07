// Package amd collects hardware metrics from AMD GPUs via rocm-smi.
package amd

import (
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/youorg/inferscope/internal/collector/common"
)

// Collector polls rocm-smi for AMD GPU metrics and implements common.HardwareCollector.
type Collector struct {
	mu       sync.RWMutex
	devices  []common.DeviceMetrics
	interval time.Duration
	stop     chan struct{}
}

// NewCollector creates and starts an AMD GPU collector.
// Returns an error if rocm-smi is not found or the initial poll fails.
func NewCollector(interval time.Duration) (*Collector, error) {
	if _, err := exec.LookPath("rocm-smi"); err != nil {
		return nil, fmt.Errorf("rocm-smi not found in PATH: %w", err)
	}
	c := &Collector{
		interval: interval,
		stop:     make(chan struct{}),
	}
	if err := c.poll(); err != nil {
		return nil, fmt.Errorf("initial AMD poll failed: %w", err)
	}
	go c.loop()
	return c, nil
}

func (c *Collector) Name() string { return "amd" }

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
				log.Printf("amd: poll error: %v", err)
			}
		case <-c.stop:
			return
		}
	}
}

// rocmSMIOutput is the top-level JSON from rocm-smi --json.
// Keys are "card0", "card1", etc.; values are flat string maps.
type rocmSMIOutput map[string]map[string]string

func (c *Collector) poll() error {
	out, err := exec.Command("rocm-smi",
		"--showuse", "--showmemuse", "--showpower",
		"--showtemp", "--showclocks", "--showmeminfo", "vram",
		"--json").Output()
	if err != nil {
		// Older rocm-smi versions don't support --showmeminfo
		out, err = exec.Command("rocm-smi",
			"--showuse", "--showpower", "--showtemp", "--showclocks",
			"--json").Output()
		if err != nil {
			return fmt.Errorf("rocm-smi failed: %w", err)
		}
	}

	var raw rocmSMIOutput
	if err := json.Unmarshal(out, &raw); err != nil {
		return fmt.Errorf("rocm-smi JSON parse: %w", err)
	}

	var devices []common.DeviceMetrics
	for key, fields := range raw {
		if !strings.HasPrefix(key, "card") {
			continue
		}
		idx, _ := strconv.Atoi(strings.TrimPrefix(key, "card"))

		name := fields["Card series"]
		if name == "" {
			name = fields["Card model"]
		}
		if name == "" {
			name = fmt.Sprintf("amd-gpu-%d", idx)
		}

		d := common.DeviceMetrics{
			Index:         idx,
			Name:          name,
			Vendor:        "amd",
			UtilPercent:   parsePercent(fields["GPU use (%)"]),
			TempCelsius:   parseTemp(fields["Temperature (Sensor edge) (C)"]),
			ClockMHz:      parseMHz(fields["sclk clock speed:"]),
			ProcessMemory: make(map[int]uint64),
		}

		d.PowerWatts = parseWatts(fields["Average Graphics Package Power (W)"])
		if d.PowerWatts == 0 {
			d.PowerWatts = parseWatts(fields["Current Socket Graphics Package Power (W)"])
		}
		if d.TempCelsius == 0 {
			d.TempCelsius = parseTemp(fields["Temperature (Sensor junction) (C)"])
		}

		d.MemUsedMiB = parseMiB(fields["VRAM Total Used Memory (B)"], fields["VRAM Used"])
		d.MemTotalMiB = parseMiB(fields["VRAM Total Memory (B)"], fields["VRAM Total"])

		devices = append(devices, d)
	}

	c.mu.Lock()
	c.devices = devices
	c.mu.Unlock()
	return nil
}

// ── value parsers ──────────────────────────────────────────────────────────

func parsePercent(s string) float64 {
	s = strings.TrimSuffix(strings.TrimSpace(s), "%")
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func parseWatts(s string) float64 {
	s = strings.TrimSuffix(strings.TrimSpace(s), "W")
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

func parseTemp(s string) float64 {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, "C")
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

func parseMHz(s string) float64 {
	s = strings.Trim(s, "()")
	s = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "mhz"))
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

// parseMiB handles both raw-bytes fields and pre-formatted MiB fields.
func parseMiB(bytesVal, mibVal string) float64 {
	if bytesVal != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(bytesVal), 64); err == nil && f > 0 {
			return f / (1024 * 1024)
		}
	}
	if mibVal != "" {
		s := strings.TrimSuffix(strings.TrimSpace(mibVal), " MiB")
		s = strings.TrimSuffix(s, "MiB")
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return f
		}
	}
	return 0
}
