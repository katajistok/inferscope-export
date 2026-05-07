// Package common defines shared types used across all vendor GPU collectors.
package common

// DeviceMetrics holds hardware counters for a single GPU, regardless of vendor.
// All vendor collectors (AMD, NVIDIA, Intel) populate this same struct so the
// exporter can handle them generically without per-vendor type assertions.
type DeviceMetrics struct {
	Index       int     // GPU index on the node (0-based)
	UUID        string  // Unique device identifier (empty if not available)
	Name        string  // Human-readable GPU model name
	Vendor      string  // "amd", "nvidia", "intel"
	UtilPercent float64 // Compute/render engine utilization %
	MemUsedMiB  float64 // VRAM used in MiB
	MemTotalMiB float64 // VRAM total in MiB
	PowerWatts  float64 // Current power draw in watts
	TempCelsius float64 // Core temperature in Celsius
	ClockMHz    float64 // Current core/shader clock in MHz

	// ProcessMemory maps PID → VRAM bytes used by that process.
	// Only populated by vendors whose driver API supports per-process VRAM
	// queries (currently NVIDIA via NVML). Empty map for AMD and Intel.
	ProcessMemory map[int]uint64
}

// HardwareCollector is the interface every vendor GPU collector must satisfy.
// The exporter loops over a []HardwareCollector and calls GetDevices() on each,
// so adding a new vendor never requires changes to exporter.go.
type HardwareCollector interface {
	// Name returns a short identifier used in logs (e.g. "amd", "nvidia", "intel").
	Name() string

	// GetDevices returns a snapshot of current hardware metrics for all GPUs
	// managed by this collector. Safe to call concurrently.
	GetDevices() []DeviceMetrics

	// Stop signals the collector's background polling goroutine to exit.
	Stop()
}
