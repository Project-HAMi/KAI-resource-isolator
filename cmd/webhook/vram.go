/*
Copyright The HAMi Authors.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"log"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// gpuMemoryNodeLabel is the per-GPU VRAM (MiB) advertised on GPU nodes by NVIDIA
// GPU Feature Discovery / Node Feature Discovery.
const gpuMemoryNodeLabel = "nvidia.com/gpu.memory"

// vramAutodetectInterval is how often the basis is refreshed (GPU nodes may join
// or change after startup).
const vramAutodetectInterval = 10 * time.Minute

// newInClusterClientset builds a clientset from the pod's service account.
func newInClusterClientset() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// detectPerGpuVramMiB returns the per-GPU VRAM basis (MiB) for gpu-fraction caps,
// read from node labels. A homogeneous cluster yields that single value; a
// heterogeneous cluster yields the minimum across GPU nodes — the cap that holds
// on whichever GPU a pod lands on, since the target GPU is unknown at admission —
// and logs the spread. Returns 0 when no GPU node advertises the label.
func detectPerGpuVramMiB(ctx context.Context, cs kubernetes.Interface) (int64, error) {
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	var minMiB int64
	seen := map[int64]int{}
	for i := range nodes.Items {
		raw := nodes.Items[i].Labels[gpuMemoryNodeLabel]
		if raw == "" {
			continue
		}
		n, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil || n <= 0 {
			continue
		}
		seen[n]++
		if minMiB == 0 || n < minMiB {
			minMiB = n
		}
	}
	if len(seen) > 1 {
		log.Printf("heterogeneous per-GPU VRAM across nodes %v MiB; using minimum %d MiB for gpu-fraction caps (set PER_GPU_VRAM_MIB to override)", seen, minMiB)
	}
	return minMiB, nil
}

// startVramAutodetect sets perGpuVramMiB from node labels immediately and then
// refreshes it periodically in the background.
func startVramAutodetect(ctx context.Context, cs kubernetes.Interface) {
	refresh := func() {
		v, err := detectPerGpuVramMiB(ctx, cs)
		if err != nil {
			log.Printf("per-GPU VRAM autodetect failed: %v (keeping %d MiB)", err, perGpuVramMiB.Load())
			return
		}
		if v > 0 {
			perGpuVramMiB.Store(v)
		}
	}

	refresh()
	if got := perGpuVramMiB.Load(); got > 0 {
		log.Printf("per-GPU VRAM basis = %d MiB (autodetected from %q node labels)", got, gpuMemoryNodeLabel)
	} else {
		log.Printf("per-GPU VRAM not detected from node labels; gpu-fraction caps disabled until PER_GPU_VRAM_MIB is set or %q appears", gpuMemoryNodeLabel)
	}

	go func() {
		t := time.NewTicker(vramAutodetectInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refresh()
			}
		}
	}()
}
