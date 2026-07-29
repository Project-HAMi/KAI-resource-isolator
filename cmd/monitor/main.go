/*
Copyright The HAMi Authors.
SPDX-License-Identifier: Apache-2.0

kai-vgpu-monitor exposes HAMi-compatible per-container VRAM metrics on :9394.
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Project-HAMi/kai-resource-isolator/pkg/monitor/nvidia"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/klog/v2"
)

// main starts kai-vgpu-monitor: env validation, container-cache refresh loop,
// Prometheus registry, and HTTP /metrics + /healthz.
//
// Pay attention:
//   - Requires CONTAINER_VGPU_MOUNT (or HOOK_PATH) and NODE_NAME.
//   - Cache refresh runs both on a ticker and again inside Collect; that is
//     intentional so scrapes see fresh data and orphan cache dirs are still
//     reclaimed when nobody scrapes. Both paths are serialised by the lister mutex.
//   - go-nvml needs CGO at build time and libnvidia-ml.so at runtime (NVIDIA
//     container toolkit / NVIDIA_VISIBLE_DEVICES).
func main() {
	metricsBindAddress := flag.String("metrics-bind-address", ":9394", "TCP address for Prometheus metrics (e.g. :9394)")
	legacyMetrics := flag.Bool("legacy-metrics", false, "Emit legacy metric names alongside hami_* names")
	updateInterval := flag.Duration("update-interval", 5*time.Second, "How often to rescan libvgpu cache directories")
	klog.InitFlags(nil)
	flag.Parse()

	if err := ValidateEnvVars(); err != nil {
		klog.Fatalf("failed to validate environment variables: %v", err)
	}

	containerLister, err := nvidia.NewContainerLister()
	if err != nil {
		klog.Fatalf("failed to create container lister: %v", err)
	}
	defer containerLister.Stop()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() {
		t := time.NewTicker(*updateInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := containerLister.Update(); err != nil {
					klog.Errorf("container lister update: %v", err)
				}
			}
		}
	}()

	reg := prometheus.NewRegistry()
	NewClusterManager("vGPU", reg, containerLister, containerLister.PodLister(), *legacyMetrics)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{Addr: *metricsBindAddress, Handler: mux}
	go func() {
		klog.Infof("kai-vgpu-monitor listening on %s", *metricsBindAddress)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			klog.Errorf("metrics server: %v", err)
			cancel()
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		klog.Errorf("metrics server shutdown: %v", err)
	}
}

// ValidateEnvVars checks required runtime environment before starting.
//
// Pay attention: CONTAINER_VGPU_MOUNT is preferred; HOOK_PATH is accepted for
// HAMi compatibility. Both must point at the same host path the webhook mounts
// (default /usr/local/vgpu). NODE_NAME must be the Kubernetes node this pod runs on.
func ValidateEnvVars() error {
	if _, err := nvidia.MountPath(); err != nil {
		return err
	}
	if os.Getenv(nvidia.NodeNameEnvName) == "" {
		return fmt.Errorf("required environment variable %s not set", nvidia.NodeNameEnvName)
	}
	return nil
}
