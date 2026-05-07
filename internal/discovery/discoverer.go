// Package discovery scans /proc to find processes that have GPU device files
// open, identifies the GPU vendor via sysfs PCI IDs, extracts the model name
// and inference runtime from the process cmdline/environ, and discovers which
// TCP ports the process listens on for opportunistic /metrics scraping.
package discovery

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Vendor represents the GPU vendor identified from sysfs PCI IDs.
type Vendor string

const (
	VendorAMD     Vendor = "amd"
	VendorNvidia  Vendor = "nvidia"
	VendorIntel   Vendor = "intel"
	VendorUnknown Vendor = "unknown"
)

// PCI vendor IDs
const (
	pciVendorAMD    = "0x1002"
	pciVendorNvidia = "0x10de"
	pciVendorIntel  = "0x8086"
)

// Runtime is a best-guess of the inference runtime inferred from process info.
type Runtime string

const (
	RuntimeOllama   Runtime = "ollama"
	RuntimeVLLM     Runtime = "vllm"
	RuntimeLlamaCpp Runtime = "llama.cpp"
	RuntimeTGI      Runtime = "tgi"
	RuntimeNIM      Runtime = "nim"
	RuntimeUnknown  Runtime = "unknown"
)

// GPUProcess represents a process that has one or more GPU devices open.
type GPUProcess struct {
	PID         int
	Comm        string  // process name from /proc/<pid>/comm
	ModelName   string  // inferred AI model name
	Runtime     Runtime // best-guess inference runtime
	Vendor      Vendor  // primary GPU vendor used by this process
	GPUIDs      []int   // GPU indices this process uses
	ListenPorts []int   // TCP ports the process listens on
	CmdLine     string  // raw cmdline (null-bytes replaced with spaces)
}

// renderNodeVendors caches /dev/dri/renderD<N> → Vendor lookups so we
// only read sysfs once per render node across all /proc scans.
var (
	renderVendorMu    sync.RWMutex
	renderVendorCache = make(map[int]Vendor) // renderD index → Vendor
)

// Discoverer continuously scans /proc for GPU-using processes.
type Discoverer struct {
	mu       sync.RWMutex
	procs    map[int]*GPUProcess
	interval time.Duration
	stop     chan struct{}
}

// NewDiscoverer creates a Discoverer that rescans every interval.
func NewDiscoverer(interval time.Duration) *Discoverer {
	d := &Discoverer{
		procs:    make(map[int]*GPUProcess),
		interval: interval,
		stop:     make(chan struct{}),
	}
	d.scan()
	go d.loop()
	return d
}

// GetProcesses returns a snapshot of currently discovered GPU processes.
func (d *Discoverer) GetProcesses() []*GPUProcess {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]*GPUProcess, 0, len(d.procs))
	for _, p := range d.procs {
		out = append(out, p)
	}
	return out
}

// Stop signals the background scan goroutine to exit.
func (d *Discoverer) Stop() { close(d.stop) }

func (d *Discoverer) loop() {
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			d.scan()
		case <-d.stop:
			return
		}
	}
}

func (d *Discoverer) scan() {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		log.Printf("discovery: cannot read /proc: %v", err)
		return
	}

	found := make(map[int]*GPUProcess)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		vendor, gpuIDs := detectGPUUsage(pid)
		if vendor == VendorUnknown {
			continue
		}
		proc := buildProcess(pid, vendor, gpuIDs)
		if proc != nil && !isNonInferenceComm(proc.Comm) {
			found[pid] = proc
		}
	}

	d.mu.Lock()
	d.procs = found
	d.mu.Unlock()

	log.Printf("discovery: found %d GPU process(es)", len(found))
}

// detectGPUUsage inspects /proc/<pid>/fd to find open GPU device files and
// determines the vendor using sysfs PCI vendor IDs — not device path prefixes.
//
// Device classification:
//   - /dev/nvidia<N>          → NVIDIA (GPU index N)
//   - /dev/nvidiactl          → NVIDIA (control, no specific GPU index)
//   - /dev/kfd               → AMD (Kernel Fusion Driver — AMD-only device)
//   - /dev/dri/renderD<N>    → vendor resolved via sysfs (AMD or Intel share DRM)
func detectGPUUsage(pid int) (Vendor, []int) {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return VendorUnknown, nil
	}

	var vendor Vendor = VendorUnknown
	gpuSet := make(map[int]struct{})

	for _, e := range entries {
		link, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err != nil {
			continue
		}

		switch {
		// NVIDIA: /dev/nvidia0, /dev/nvidia1, ...
		// /dev/nvidiactl is the control device (no GPU index)
		case strings.HasPrefix(link, "/dev/nvidia"):
			suffix := strings.TrimPrefix(link, "/dev/nvidia")
			if idx, err := strconv.Atoi(suffix); err == nil {
				vendor = VendorNvidia
				gpuSet[idx] = struct{}{}
			} else if suffix == "ctl" || suffix == "-uvm" {
				// Control/UVM device — mark as NVIDIA but no specific GPU index
				if vendor == VendorUnknown {
					vendor = VendorNvidia
				}
			}

		// AMD: /dev/kfd is exclusively AMD's Kernel Fusion Driver
		case link == "/dev/kfd":
			vendor = VendorAMD

		// DRM render nodes: used by both AMD and Intel — must resolve via sysfs
		case strings.HasPrefix(link, "/dev/dri/renderD"):
			idxStr := strings.TrimPrefix(link, "/dev/dri/renderD")
			renderIdx, err := strconv.Atoi(idxStr)
			if err != nil {
				continue
			}
			nodeVendor := renderNodeVendor(renderIdx)
			if nodeVendor != VendorUnknown {
				// Only override vendor if we haven't seen a more specific signal
				// (e.g. /dev/kfd already confirmed AMD for this process)
				if vendor == VendorUnknown {
					vendor = nodeVendor
				}
				gpuSet[renderIdx-128] = struct{}{}
			}
		}
	}

	// Supplement GPU index list from environment variables
	for _, id := range parseVisibleDevicesFromEnv(pid) {
		gpuSet[id] = struct{}{}
	}

	ids := make([]int, 0, len(gpuSet))
	for id := range gpuSet {
		ids = append(ids, id)
	}
	return vendor, ids
}

// renderNodeVendor resolves the GPU vendor for a DRM render node by reading
// the PCI vendor ID from sysfs. Results are cached for the lifetime of the process.
// renderIdx is the raw number from /dev/dri/renderD<renderIdx>.
func renderNodeVendor(renderIdx int) Vendor {
	renderVendorMu.RLock()
	if v, ok := renderVendorCache[renderIdx]; ok {
		renderVendorMu.RUnlock()
		return v
	}
	renderVendorMu.RUnlock()

	// /sys/class/drm/renderD<N>/device/vendor contains the PCI vendor ID
	path := fmt.Sprintf("/sys/class/drm/renderD%d/device/vendor", renderIdx)
	data, err := os.ReadFile(path)
	var v Vendor
	if err != nil {
		v = VendorUnknown
	} else {
		switch strings.TrimSpace(string(data)) {
		case pciVendorAMD:
			v = VendorAMD
		case pciVendorIntel:
			v = VendorIntel
		case pciVendorNvidia:
			v = VendorNvidia
		default:
			v = VendorUnknown
		}
	}

	renderVendorMu.Lock()
	renderVendorCache[renderIdx] = v
	renderVendorMu.Unlock()
	return v
}

func buildProcess(pid int, vendor Vendor, gpuIDs []int) *GPUProcess {
	comm := strings.TrimSpace(readFile(fmt.Sprintf("/proc/%d/comm", pid)))
	cmdlineRaw := readFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	cmdline := strings.TrimSpace(strings.ReplaceAll(cmdlineRaw, "\x00", " "))
	environ := readFile(fmt.Sprintf("/proc/%d/environ", pid))

	return &GPUProcess{
		PID:         pid,
		Comm:        comm,
		ModelName:   extractModelName(cmdline, environ, comm),
		Runtime:     detectRuntime(cmdline, comm, environ),
		Vendor:      vendor,
		GPUIDs:      gpuIDs,
		ListenPorts: findListeningPorts(pid),
		CmdLine:     cmdline,
	}
}

// extractModelName tries multiple heuristics to find the AI model name.
func extractModelName(cmdline, environ, comm string) string {
	args := strings.Fields(cmdline)

	// Named flags that precede the model path
	modelFlags := []string{
		"--model", "-m", "--model-path", "--model_path",
		"--checkpoint", "-model", "--served-model-name",
	}
	for i, arg := range args {
		for _, flag := range modelFlags {
			if arg == flag && i+1 < len(args) {
				return cleanModelName(args[i+1])
			}
			if strings.HasPrefix(arg, flag+"=") {
				return cleanModelName(strings.SplitN(arg, "=", 2)[1])
			}
		}
	}

	// Environment variable fallbacks
	for _, line := range strings.Split(environ, "\x00") {
		for _, v := range []string{"OLLAMA_MODEL", "MODEL_NAME", "MODEL_ID", "HF_MODEL_ID", "NIM_MODEL_NAME"} {
			if strings.HasPrefix(line, v+"=") {
				return cleanModelName(strings.SplitN(line, "=", 2)[1])
			}
		}
	}

	// File extension heuristic
	for _, arg := range args {
		if strings.Contains(arg, ".gguf") || strings.Contains(arg, ".bin") ||
			strings.Contains(arg, ".safetensors") {
			return cleanModelName(arg)
		}
	}

	return comm
}

func cleanModelName(raw string) string {
	name := filepath.Base(raw)
	for _, ext := range []string{".gguf", ".bin", ".safetensors", ".pt"} {
		name = strings.TrimSuffix(name, ext)
	}
	if name == "" || name == "." {
		return raw
	}
	return name
}

// detectRuntime identifies the inference runtime from process cmdline and name.
func detectRuntime(cmdline, comm, environ string) Runtime {
	lower := strings.ToLower(cmdline + " " + comm)
	switch {
	case strings.Contains(lower, "nim") ||
		strings.Contains(strings.ToLower(environ), "nim_model_name"):
		return RuntimeNIM
	case strings.Contains(lower, "ollama"):
		return RuntimeOllama
	case strings.Contains(lower, "vllm") || strings.Contains(lower, "vllm_worker"):
		return RuntimeVLLM
	case strings.Contains(lower, "llama-server") ||
		strings.Contains(lower, "llama.cpp") ||
		strings.Contains(lower, "llama-cpp") ||
		(strings.Contains(lower, "server") && strings.Contains(lower, "llama")):
		return RuntimeLlamaCpp
	case strings.Contains(lower, "text-generation") || strings.Contains(lower, "tgi"):
		return RuntimeTGI
	}
	return RuntimeUnknown
}

// findListeningPorts returns only the TCP ports whose listening sockets are
// actually owned by pid. It works in two steps:
//
//  1. Collect the socket inodes that appear in /proc/<pid>/fd/ (the sockets
//     this process has open).
//
//  2. Scan /proc/<pid>/net/tcp{,6} for LISTEN entries whose inode (column 9)
//     appears in that set.
//
// This is correct across network namespaces: two processes in different
// namespaces share their own net/tcp view, and inode cross-referencing
// ensures we only claim ports the process itself is listening on.
func findListeningPorts(pid int) []int {
	ownedInodes := socketInodes(pid)
	if len(ownedInodes) == 0 {
		return nil
	}

	var ports []int
	seen := make(map[int]struct{})

	for _, netFile := range []string{
		fmt.Sprintf("/proc/%d/net/tcp", pid),
		fmt.Sprintf("/proc/%d/net/tcp6", pid),
	} {
		f, err := os.Open(netFile)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Scan() // skip header
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			// /proc/net/tcp columns:
			//   0=sl 1=local_addr 2=rem_addr 3=state 4=tx:rx 5=tr:tm 6=retrnsmt 7=uid 8=timeout 9=inode
			if len(fields) < 10 || fields[3] != "0A" { // 0A = TCP_LISTEN
				continue
			}
			inode, err := strconv.ParseUint(fields[9], 10, 64)
			if err != nil {
				continue
			}
			if _, owned := ownedInodes[inode]; !owned {
				continue
			}
			parts := strings.Split(fields[1], ":")
			if len(parts) < 2 {
				continue
			}
			port64, err := strconv.ParseInt(parts[len(parts)-1], 16, 32)
			if err != nil {
				continue
			}
			port := int(port64)
			if _, ok := seen[port]; !ok {
				seen[port] = struct{}{}
				ports = append(ports, port)
			}
		}
		f.Close()
	}
	return ports
}

// socketInodes returns the set of socket inodes held open by pid, by reading
// the symlinks in /proc/<pid>/fd/ and extracting the inode from "socket:[N]".
func socketInodes(pid int) map[uint64]struct{} {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil
	}
	inodes := make(map[uint64]struct{})
	for _, e := range entries {
		link, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err != nil {
			continue
		}
		// socket:[12345]
		if !strings.HasPrefix(link, "socket:[") || !strings.HasSuffix(link, "]") {
			continue
		}
		inode, err := strconv.ParseUint(link[8:len(link)-1], 10, 64)
		if err != nil {
			continue
		}
		inodes[inode] = struct{}{}
	}
	return inodes
}

func parseVisibleDevicesFromEnv(pid int) []int {
	environ := readFile(fmt.Sprintf("/proc/%d/environ", pid))
	var ids []int
	for _, line := range strings.Split(environ, "\x00") {
		for _, key := range []string{
			"HIP_VISIBLE_DEVICES",
			"CUDA_VISIBLE_DEVICES",
			"ROCR_VISIBLE_DEVICES",
			"ONEAPI_DEVICE_SELECTOR", // Intel oneAPI
		} {
			if strings.HasPrefix(line, key+"=") {
				val := strings.SplitN(line, "=", 2)[1]
				for _, part := range strings.Split(val, ",") {
					part = strings.TrimSpace(part)
					// Intel oneAPI uses "level_zero:0" format — extract the number
					if strings.Contains(part, ":") {
						part = strings.SplitN(part, ":", 2)[1]
					}
					if idx, err := strconv.Atoi(part); err == nil {
						ids = append(ids, idx)
					}
				}
			}
		}
	}
	return ids
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// isNonInferenceComm returns true for well-known processes that open GPU
// device files for non-inference reasons (display servers, the exporter itself).
func isNonInferenceComm(comm string) bool {
	switch comm {
	case "Xorg", "Xwayland", "Xvfb", "axon":
		return true
	}
	return false
}

// IsPortOpen does a fast TCP dial to verify a port is actively serving.
func IsPortOpen(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
