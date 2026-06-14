/*
Copyright The HAMi Authors.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"log"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// gpuMemoryNodeLabel is the per-GPU VRAM (MiB) advertised on GPU nodes by NVIDIA
// GPU Feature Discovery / Node Feature Discovery.
const gpuMemoryNodeLabel = "nvidia.com/gpu.memory"

// newInClusterClientset builds a clientset from the pod's service account.
func newInClusterClientset() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// gpuMemoryMiBFromNodes returns the per-GPU VRAM basis (minimum across GPU nodes,
// the cap that holds on whichever GPU a pod lands on since the target is unknown at
// admission) and the set of distinct values seen (for heterogeneity logging).
// Returns 0 when no node advertises the label.
func gpuMemoryMiBFromNodes(nodes []*corev1.Node) (int64, map[int64]int) {
	var minMiB int64
	seen := map[int64]int{}
	for _, n := range nodes {
		raw := n.Labels[gpuMemoryNodeLabel]
		if raw == "" {
			continue
		}
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v <= 0 {
			continue
		}
		seen[v]++
		if minMiB == 0 || v < minMiB {
			minMiB = v
		}
	}
	return minMiB, seen
}

// detectPerGpuVramMiB reads the per-GPU VRAM basis directly via the API (one List).
// Kept for the standalone live test; the running webhook uses the informer below.
func detectPerGpuVramMiB(ctx context.Context, cs kubernetes.Interface) (int64, error) {
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	ptrs := make([]*corev1.Node, 0, len(nodes.Items))
	for i := range nodes.Items {
		ptrs = append(ptrs, &nodes.Items[i])
	}
	minMiB, seen := gpuMemoryMiBFromNodes(ptrs)
	if len(seen) > 1 {
		log.Printf("heterogeneous per-GPU VRAM across nodes %v MiB; using minimum %d MiB (set PER_GPU_VRAM_MIB to override)", seen, minMiB)
	}
	return minMiB, nil
}

// startVramAutodetect keeps perGpuVramMiB up to date from node labels using a
// Node informer (watch). It recomputes the basis on node add/update/delete events
// instead of polling, reading from the informer's in-memory cache (no repeated
// List calls). Blocks until the cache has synced, then returns.
func startVramAutodetect(ctx context.Context, cs kubernetes.Interface) {
	factory := informers.NewSharedInformerFactory(cs, 0) // 0 = event-driven, no periodic resync
	nodeInformer := factory.Core().V1().Nodes()
	lister := nodeInformer.Lister()

	refresh := func() {
		nodes, err := lister.List(labels.Everything())
		if err != nil {
			log.Printf("per-GPU VRAM refresh failed: %v (keeping %d MiB)", err, perGpuVramMiB.Load())
			return
		}
		minMiB, seen := gpuMemoryMiBFromNodes(nodes)
		if minMiB > 0 {
			perGpuVramMiB.Store(minMiB)
		}
		if len(seen) > 1 {
			log.Printf("heterogeneous per-GPU VRAM across nodes %v MiB; using minimum %d MiB (set PER_GPU_VRAM_MIB to override)", seen, minMiB)
		}
	}

	if _, err := nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { refresh() },
		UpdateFunc: func(interface{}, interface{}) { refresh() }, // labels can change
		DeleteFunc: func(interface{}) { refresh() },
	}); err != nil {
		log.Printf("failed to register node informer handler: %v; set PER_GPU_VRAM_MIB to enable gpu-fraction caps", err)
		return
	}

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), nodeInformer.Informer().HasSynced) {
		log.Printf("node informer cache sync failed; gpu-fraction caps unset until PER_GPU_VRAM_MIB set or nodes sync")
		return
	}
	refresh() // initial value after sync

	if got := perGpuVramMiB.Load(); got > 0 {
		log.Printf("per-GPU VRAM basis = %d MiB (autodetected via node informer on %q)", got, gpuMemoryNodeLabel)
	} else {
		log.Printf("per-GPU VRAM not detected from node labels; gpu-fraction caps disabled until PER_GPU_VRAM_MIB set or %q appears", gpuMemoryNodeLabel)
	}
}
