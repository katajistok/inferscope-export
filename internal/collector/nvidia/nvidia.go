// Package nvidia collects hardware metrics from NVIDIA GPUs via NVML (go-nvml).
package nvidia

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/youorg/inferscope/internal/collector/common"
)

// energySample stores a single energy reading for power-via-derivative fallback.
type energySample struct {
	energyMJ uint64
	at       time.Time
}

// Collector polls NVML for NVIDIA GPU metrics and implements common.HardwareCollector.
type Collector struct {
	mu          sync.RWMutex
	devices     []common.DeviceMetrics
	interval    time.Duration
	stop        chan struct{}
	prevEnergy  map[int]energySample // GPU index → last energy sample
}

// NewCollector initialises NVML and starts a background polling goroutine.
// Returns an error if NVML is unavailable (no NVIDIA driver installed).
func NewCollector(interval time.Duration) (*Collector, error) {
	if ret := nvml.Init(); ret != nvml.SUCCESS {
		return nil, fmt.Errorf("nvml.Init failed: %v", nvml.ErrorString(ret))
	}
	c := &Collector{
		interval:   interval,
		stop:       make(chan struct{}),
		prevEnergy: make(map[int]energySample),
	}
	if err := c.poll(); err != nil {
		nvml.Shutdown()
		return nil, fmt.Errorf("initial NVIDIA poll failed: %w", err)
	}
	go c.loop()
	return c, nil
}

func (c *Collector) Name() string { return "nvidia" }

func (c *Collector) GetDevices() []common.DeviceMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]common.DeviceMetrics, len(c.devices))
	copy(out, c.devices)
	return out
}

func (c *Collector) Stop() {
	close(c.stop)
	nvml.Shutdown()
}

func (c *Collector) loop() {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := c.poll(); err != nil {
				log.Printf("nvidia: poll error: %v", err)
			}
		case <-c.stop:
			return
		}
	}
}

func (c *Collector) poll() error {
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("DeviceGetCount: %v", nvml.ErrorString(ret))
	}

	devices := make([]common.DeviceMetrics, 0, count)

	for i := 0; i < count; i++ {
		dev, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			log.Printf("nvidia: cannot get device %d: %v", i, nvml.ErrorString(ret))
			continue
		}

		d := common.DeviceMetrics{
			Index:         i,
			Vendor:        "nvidia",
			ProcessMemory: make(map[int]uint64),
		}

		if name, ret := dev.GetName(); ret == nvml.SUCCESS {
			d.Name = name
		}
		if uuid, ret := dev.GetUUID(); ret == nvml.SUCCESS {
			d.UUID = uuid
		}
		if util, ret := dev.GetUtilizationRates(); ret == nvml.SUCCESS {
			d.UtilPercent = float64(util.Gpu)
		}
		if mem, ret := dev.GetMemoryInfo(); ret == nvml.SUCCESS {
			d.MemUsedMiB = float64(mem.Used) / (1024 * 1024)
			d.MemTotalMiB = float64(mem.Total) / (1024 * 1024)
		}
		// Primary: NVML instantaneous power (milliwatts). Not supported on all GPUs
		// (e.g. RTX A1000 mobile). Fall back to energy counter derivative.
		if power, ret := dev.GetPowerUsage(); ret == nvml.SUCCESS {
			d.PowerWatts = float64(power) / 1000.0
		} else if energy, ret := dev.GetTotalEnergyConsumption(); ret == nvml.SUCCESS {
			now := time.Now()
			if prev, ok := c.prevEnergy[i]; ok {
				deltaJ := float64(energy-prev.energyMJ) / 1000.0
				deltaSec := now.Sub(prev.at).Seconds()
				if deltaSec > 0 && energy >= prev.energyMJ {
					d.PowerWatts = deltaJ / deltaSec
				}
			}
			c.prevEnergy[i] = energySample{energyMJ: energy, at: now}
		}
		if temp, ret := dev.GetTemperature(nvml.TEMPERATURE_GPU); ret == nvml.SUCCESS {
			d.TempCelsius = float64(temp)
		}
		if clock, ret := dev.GetClockInfo(nvml.CLOCK_GRAPHICS); ret == nvml.SUCCESS {
			d.ClockMHz = float64(clock)
		}
		// Per-process VRAM — NVIDIA only feature via NVML
		if procs, ret := dev.GetComputeRunningProcesses(); ret == nvml.SUCCESS {
			for _, p := range procs {
				d.ProcessMemory[int(p.Pid)] = p.UsedGpuMemory
			}
		}

		devices = append(devices, d)
	}

	c.mu.Lock()
	c.devices = devices
	c.mu.Unlock()
	return nil
}
