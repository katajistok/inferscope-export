// Axon is the node agent component of Inferscope.
// It discovers GPU processes via /proc, collects hardware metrics from AMD,
// NVIDIA, and Intel GPUs, scrapes token metrics from inference runtimes, and
// exposes everything as Prometheus metrics on :9101/metrics.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/youorg/inferscope/internal/collector/amd"
	"github.com/youorg/inferscope/internal/collector/common"
	"github.com/youorg/inferscope/internal/collector/intel"
	"github.com/youorg/inferscope/internal/collector/nvidia"
	"github.com/youorg/inferscope/internal/collector/tokens"
	"github.com/youorg/inferscope/internal/discovery"
	"github.com/youorg/inferscope/internal/exporter"
)

func main() {
	listenAddr   := flag.String("listen-address", ":9101", "Address to expose /metrics on")
	pollInterval := flag.Duration("poll-interval", 5*time.Second, "Process discovery + token scrape interval")
	hwInterval   := flag.Duration("hw-interval", 2*time.Second, "Hardware counter poll interval")
	enableAMD    := flag.Bool("amd", true, "Enable AMD GPU collection via rocm-smi")
	enableNvidia := flag.Bool("nvidia", true, "Enable NVIDIA GPU collection via NVML")
	enableIntel  := flag.Bool("intel", true, "Enable Intel GPU collection via intel_gpu_top")
	flag.Parse()

	log.Printf("axon starting — listen=%s poll=%s hw=%s amd=%v nvidia=%v intel=%v",
		*listenAddr, *pollInterval, *hwInterval, *enableAMD, *enableNvidia, *enableIntel)

	disc := discovery.NewDiscoverer(*pollInterval)

	// Collect hardware collectors — all implement common.HardwareCollector.
	// The exporter handles them generically; adding a new vendor here is the
	// only change required when a fourth vendor is supported.
	var hwCollectors []common.HardwareCollector

	if *enableAMD {
		if c, err := amd.NewCollector(*hwInterval); err != nil {
			log.Printf("AMD collector unavailable: %v", err)
		} else {
			hwCollectors = append(hwCollectors, c)
			log.Println("AMD collector: enabled")
		}
	}

	if *enableNvidia {
		if c, err := nvidia.NewCollector(*hwInterval); err != nil {
			log.Printf("NVIDIA collector unavailable: %v", err)
		} else {
			hwCollectors = append(hwCollectors, c)
			log.Println("NVIDIA collector: enabled")
		}
	}

	if *enableIntel {
		if c, err := intel.NewCollector(*hwInterval); err != nil {
			log.Printf("Intel collector unavailable: %v", err)
		} else {
			hwCollectors = append(hwCollectors, c)
			log.Println("Intel collector: enabled")
		}
	}

	if len(hwCollectors) == 0 {
		log.Println("warning: no hardware collectors enabled — only token metrics will be collected")
	}

	tokenCol := tokens.NewCollector(disc, *pollInterval)

	reg := prometheus.NewRegistry()
	exp := exporter.New(disc, hwCollectors, tokenCol)
	reg.MustRegister(exp)

	http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	log.Printf("axon ready — metrics at %s/metrics", *listenAddr)
	log.Fatal(http.ListenAndServe(*listenAddr, nil))
}
