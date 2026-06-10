/*
Copyright The HAMi Authors.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestDetectPerGpuVramMiBLive verifies per-GPU VRAM autodetection against the
// current kube context. It is skipped when no kubeconfig/cluster is reachable, so
// it is safe in CI; run it against a GPU cluster to validate label detection.
func TestDetectPerGpuVramMiBLive(t *testing.T) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("no home dir: %v", err)
		}
		kubeconfig = filepath.Join(home, ".kube", "config")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Skipf("no usable kubeconfig (%s): %v", kubeconfig, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Skipf("clientset: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	v, err := detectPerGpuVramMiB(ctx, cs)
	if err != nil {
		t.Skipf("cluster unreachable: %v", err)
	}
	t.Logf("autodetected per-GPU VRAM = %d MiB", v)
	if v <= 0 {
		t.Fatalf("expected a positive per-GPU VRAM basis from node labels, got %d", v)
	}
}
