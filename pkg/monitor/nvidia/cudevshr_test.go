/*
Copyright The HAMi Authors.
SPDX-License-Identifier: Apache-2.0
*/

package nvidia

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	v1 "github.com/Project-HAMi/kai-resource-isolator/pkg/monitor/nvidia/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

func TestIsGPUSharingPod(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{name: "nil", pod: nil, want: false},
		{name: "no annotations", pod: &corev1.Pod{}, want: false},
		{
			name: "gpu-fraction",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				GPUFractionAnnotation: "0.5",
			}}},
			want: true,
		},
		{
			name: "gpu-memory",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				GPUMemoryAnnotation: "2048",
			}}},
			want: true,
		},
		{
			name: "unrelated annotation",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				"foo": "bar",
			}}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsGPUSharingPod(tt.pod); got != tt.want {
				t.Fatalf("IsGPUSharingPod() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseCacheFileName(t *testing.T) {
	tests := []struct {
		name          string
		file          string
		wantUID       string
		wantContainer string
		wantOK        bool
	}{
		{name: "simple", file: "abc-uid_cuda", wantUID: "abc-uid", wantContainer: "cuda", wantOK: true},
		{name: "container with underscore", file: "abc-uid_my_ctr", wantUID: "abc-uid", wantContainer: "my_ctr", wantOK: true},
		{name: "missing separator", file: "nouid", wantOK: false},
		{name: "empty container", file: "uid_", wantOK: false},
		{name: "empty uid", file: "_ctr", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uid, ctr, ok := parseCacheFileName(tt.file)
			if ok != tt.wantOK {
				t.Fatalf("ok=%v want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if uid != tt.wantUID || ctr != tt.wantContainer {
				t.Fatalf("got (%q,%q) want (%q,%q)", uid, ctr, tt.wantUID, tt.wantContainer)
			}
		})
	}
}

// writeV1Cache writes a shared-region image the way libvgpu does: full region size,
// with the header magic and version set once the region is initialised.
// The header is little-endian, matching the x86_64/arm64 nodes this runs on.
func writeV1Cache(t *testing.T, path string, initialized bool) {
	t.Helper()
	buf := make([]byte, v1.SpecSize())
	if initialized {
		binary.LittleEndian.PutUint32(buf[0:], uint32(SharedRegionMagicFlag))
		binary.LittleEndian.PutUint32(buf[4:], 1)
		binary.LittleEndian.PutUint32(buf[8:], 2)
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("failed to write cache image %s: %v", path, err)
	}
}

func unmap(t *testing.T, usage *ContainerUsage) {
	t.Helper()
	if usage == nil {
		return
	}
	if err := syscall.Munmap(usage.data); err != nil {
		t.Errorf("failed to munmap test cache: %v", err)
	}
}

func newTestLister(t *testing.T, containerPath string, pods ...*corev1.Pod) *ContainerLister {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, pod := range pods {
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("failed to seed pod indexer: %v", err)
		}
	}
	return &ContainerLister{
		containerPath: containerPath,
		containers:    make(map[string]*ContainerUsage),
		nodeName:      "gpu-node-1",
		podLister:     corelisters.NewPodLister(indexer),
	}
}

func podWithUID(uid string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "job-" + uid,
		Namespace: "team-a",
		UID:       types.UID(uid),
	}}
}

// TestLoadCacheFlatFile covers HAMi-core #219: libvgpu writes the shared region
// directly to containers/{podUID}_{containerName}.
func TestLoadCacheFlatFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uid-1_trainer")
	writeV1Cache(t, path, true)

	usage, err := loadCache(path)
	if err != nil {
		t.Fatalf("loadCache() error = %v", err)
	}
	if usage == nil || usage.Info == nil {
		t.Fatal("expected the flat cache file to be mapped")
	}
	defer unmap(t, usage)

	if got := usage.Info.DeviceNum(); got != 0 {
		t.Errorf("DeviceNum() = %d, want 0 for a freshly initialised region", got)
	}
}

func TestLoadCacheNotReady(t *testing.T) {
	root := t.TempDir()

	uninitialized := filepath.Join(root, "uid-2_trainer")
	writeV1Cache(t, uninitialized, false)

	tooSmall := filepath.Join(root, "uid-3_trainer")
	if err := os.WriteFile(tooSmall, []byte{1, 2}, 0o600); err != nil {
		t.Fatalf("failed to write short cache file: %v", err)
	}

	tests := []struct {
		name string
		path string
	}{
		{name: "cache file without magic", path: uninitialized},
		{name: "cache file not sized yet", path: tooSmall},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage, err := loadCache(tt.path)
			if err != nil {
				t.Fatalf("loadCache() error = %v, want nil so Update retries later", err)
			}
			if usage != nil {
				unmap(t, usage)
				t.Fatal("expected no usage for an uninitialised cache")
			}
		})
	}
}

// TestLoadCacheTruncated guards the unsafe cast: a region shorter than the
// struct must be rejected instead of read past the end of the mapping.
func TestLoadCacheTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uid-1_trainer")
	buf := make([]byte, 4096)
	binary.LittleEndian.PutUint32(buf[0:], uint32(SharedRegionMagicFlag))
	binary.LittleEndian.PutUint32(buf[4:], 1)
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("failed to write truncated cache: %v", err)
	}

	usage, err := loadCache(path)
	if usage != nil {
		unmap(t, usage)
		t.Fatal("expected a truncated cache to be rejected")
	}
	if err == nil || !strings.Contains(err.Error(), "shared region needs") {
		t.Fatalf("loadCache() error = %v, want a truncated shared region error", err)
	}
}

func TestUpdateDiscoversFlatCacheFiles(t *testing.T) {
	root := t.TempDir()
	writeV1Cache(t, filepath.Join(root, "uid-1_trainer"), true)
	writeV1Cache(t, filepath.Join(root, "uid-2_sidecar"), true)

	// HAMi device-plugin directory layout must be ignored for KAI.
	dir := filepath.Join(root, "uid-3_ignored")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}
	writeV1Cache(t, filepath.Join(dir, "GPU-0.cache"), true)
	if err := os.WriteFile(filepath.Join(root, "not-a-cache"), []byte("x"), 0o600); err != nil {
		t.Fatalf("failed to write unrelated file: %v", err)
	}

	lister := newTestLister(t, root, podWithUID("uid-1"), podWithUID("uid-2"), podWithUID("uid-3"))
	if err := lister.Update(); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	t.Cleanup(func() {
		for name := range lister.containers {
			lister.dropContainer(name)
		}
	})

	got := map[string]string{}
	lister.WithContainers(func(containers map[string]*ContainerUsage) {
		for key, usage := range containers {
			got[key] = usage.PodUID + "/" + usage.ContainerName
		}
	})
	want := map[string]string{
		"uid-1_trainer": "uid-1/trainer",
		"uid-2_sidecar": "uid-2/sidecar",
	}
	if len(got) != len(want) {
		t.Fatalf("discovered caches = %v, want %v", got, want)
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("cache %s = %q, want %q", key, got[key], value)
		}
	}
}

// TestUpdateRemovesOrphanCaches covers cleanup once the pod UID is gone and the
// flat cache file is older than the resync grace window.
func TestUpdateRemovesOrphanCaches(t *testing.T) {
	root := t.TempDir()
	flat := filepath.Join(root, "uid-gone_trainer")
	writeV1Cache(t, flat, true)

	stale := time.Now().Add(-2 * resyncInterval)
	if err := os.Chtimes(flat, stale, stale); err != nil {
		t.Fatalf("failed to age %s: %v", flat, err)
	}

	lister := newTestLister(t, root)
	if err := lister.Update(); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	if _, err := os.Stat(flat); !os.IsNotExist(err) {
		t.Errorf("orphan cache %s still exists (stat err = %v)", flat, err)
	}
	lister.WithContainers(func(containers map[string]*ContainerUsage) {
		if len(containers) != 0 {
			t.Errorf("expected no tracked caches, got %d", len(containers))
		}
	})
}

// TestUpdateConcurrentWithReads is the regression test for the collector racing the
// refresh ticker: reads must be serialised with Update, which unmaps entries.
func TestUpdateConcurrentWithReads(t *testing.T) {
	root := t.TempDir()
	writeV1Cache(t, filepath.Join(root, "uid-1_trainer"), true)
	lister := newTestLister(t, root, podWithUID("uid-1"))
	t.Cleanup(func() {
		for name := range lister.containers {
			lister.dropContainer(name)
		}
	})

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 50 {
				if err := lister.Update(); err != nil {
					t.Errorf("Update() error = %v", err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 50 {
				lister.WithContainers(func(containers map[string]*ContainerUsage) {
					for _, usage := range containers {
						_ = usage.Info.DeviceNum()
					}
				})
			}
		}()
	}
	wg.Wait()
}
