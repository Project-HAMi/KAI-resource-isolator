/*
Copyright The HAMi Authors.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"fmt"
	"testing"

	"github.com/Project-HAMi/kai-resource-isolator/pkg/monitor/nvidia"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	listerscorev1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// validUUID is 40 chars, the length collectContainerMetrics keeps from the
// 96-byte libvgpu buffer.
const validUUID = "GPU-00000000-0000-0000-0000-000000000000"

// containerMetricNames are the per-container series. Host GPU series are not listed:
// they come from NVML, so they appear or not depending on the test machine.
var containerMetricNames = []string{
	"hami_vgpu_memory_used_bytes",
	"hami_vgpu_memory_limit_bytes",
	"hami_vgpu_memory_context_bytes",
	"hami_vgpu_memory_module_bytes",
	"hami_vgpu_memory_buffer_bytes",
	"hami_container_device_memory_bytes",
	"hami_container_device_utilization_ratio",
	"hami_container_last_kernel_elapsed_seconds",
}

// fakeUsage is a UsageInfo that returns fixed values instead of reading mmap'd memory.
type fakeUsage struct {
	devices        int
	uuid           string
	total          uint64
	limit          uint64
	contextSize    uint64
	moduleSize     uint64
	bufferSize     uint64
	smUtil         uint64
	lastKernelTime int64
}

func (f *fakeUsage) DeviceMax() int                     { return 16 }
func (f *fakeUsage) DeviceNum() int                     { return f.devices }
func (f *fakeUsage) DeviceMemoryContextSize(int) uint64 { return f.contextSize }
func (f *fakeUsage) DeviceMemoryModuleSize(int) uint64  { return f.moduleSize }
func (f *fakeUsage) DeviceMemoryBufferSize(int) uint64  { return f.bufferSize }
func (f *fakeUsage) DeviceMemoryOffset(int) uint64      { return 0 }
func (f *fakeUsage) DeviceMemoryTotal(int) uint64       { return f.total }
func (f *fakeUsage) DeviceSmUtil(int) uint64            { return f.smUtil }
func (f *fakeUsage) SetDeviceSmLimit(uint64)            {}
func (f *fakeUsage) IsValidUUID(int) bool               { return f.uuid != "" }
func (f *fakeUsage) DeviceUUID(int) string              { return f.uuid }
func (f *fakeUsage) DeviceMemoryLimit(int) uint64       { return f.limit }
func (f *fakeUsage) SetDeviceMemoryLimit(uint64)        {}
func (f *fakeUsage) LastKernelTime() int64              { return f.lastKernelTime }
func (f *fakeUsage) GetPriority() int                   { return 0 }
func (f *fakeUsage) GetRecentKernel() int32             { return 0 }
func (f *fakeUsage) SetRecentKernel(int32)              {}
func (f *fakeUsage) GetUtilizationSwitch() int32        { return 0 }
func (f *fakeUsage) SetUtilizationSwitch(int32)         {}

// fakeSource is a containerSource backed by an in-memory map.
type fakeSource struct {
	containers map[string]*nvidia.ContainerUsage
	updates    int
}

func (f *fakeSource) Update() error {
	f.updates++
	return nil
}

func (f *fakeSource) WithContainers(fn func(map[string]*nvidia.ContainerUsage)) {
	fn(f.containers)
}

func (f *fakeSource) NodeName() string { return "gpu-node-1" }

func TestCollectPerContainerMetrics(t *testing.T) {
	usage := &fakeUsage{
		devices:     1,
		uuid:        validUUID,
		total:       3 << 30,
		limit:       4 << 30,
		contextSize: 1 << 20,
		moduleSize:  2 << 20,
		bufferSize:  3 << 20,
		smUtil:      42,
	}
	source := &fakeSource{containers: map[string]*nvidia.ContainerUsage{
		"uid-1_trainer": {PodUID: "uid-1", ContainerName: "trainer", Info: usage},
	}}
	reg := prometheus.NewRegistry()
	NewClusterManager("vGPU", reg, source, podLister(t, gpuSharingPod("uid-1", "trainer")), false)

	families := gather(t, reg)

	if source.updates != 1 {
		t.Fatalf("expected the scrape to refresh the cache once, got %d refreshes", source.updates)
	}

	used := requireSingleSample(t, families, "hami_vgpu_memory_used_bytes")
	if used.value != float64(usage.total) {
		t.Errorf("hami_vgpu_memory_used_bytes = %v, want %v", used.value, float64(usage.total))
	}
	wantLabels := map[string]string{
		"zone":          "vGPU",
		"namespace":     "team-a",
		"pod":           "job-0",
		"container":     "trainer",
		"vdevice_index": "0",
		"device_uuid":   validUUID,
	}
	for k, want := range wantLabels {
		if got := used.labels[k]; got != want {
			t.Errorf("label %s = %q, want %q", k, got, want)
		}
	}

	for name, want := range map[string]float64{
		"hami_vgpu_memory_limit_bytes":            float64(usage.limit),
		"hami_vgpu_memory_context_bytes":          float64(usage.contextSize),
		"hami_vgpu_memory_module_bytes":           float64(usage.moduleSize),
		"hami_vgpu_memory_buffer_bytes":           float64(usage.bufferSize),
		"hami_container_device_utilization_ratio": float64(usage.smUtil),
	} {
		if got := requireSingleSample(t, families, name).value; got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}

	// lastKernelTime 0 (v0 caches, or no kernel yet) must not produce a series.
	if _, ok := families["hami_container_last_kernel_elapsed_seconds"]; ok {
		t.Error("hami_container_last_kernel_elapsed_seconds was emitted for an unset last kernel time")
	}

	// The breakdown metric carries the offset remainder in a label.
	breakdown := requireSingleSample(t, families, "hami_container_device_memory_bytes")
	wantOffset := fmt.Sprint(usage.total - usage.contextSize - usage.moduleSize - usage.bufferSize)
	if got := breakdown.labels["offset"]; got != wantOffset {
		t.Errorf("offset label = %q, want %q", got, wantOffset)
	}
}

func TestCollectLastKernelElapsed(t *testing.T) {
	source := &fakeSource{containers: map[string]*nvidia.ContainerUsage{
		"uid-1_trainer": {PodUID: "uid-1", ContainerName: "trainer", Info: &fakeUsage{
			devices: 1, uuid: validUUID, lastKernelTime: 1,
		}},
	}}
	reg := prometheus.NewRegistry()
	NewClusterManager("vGPU", reg, source, podLister(t, gpuSharingPod("uid-1", "trainer")), false)

	elapsed := requireSingleSample(t, gather(t, reg), "hami_container_last_kernel_elapsed_seconds")
	if elapsed.value <= 0 {
		t.Errorf("elapsed seconds = %v, want a positive age", elapsed.value)
	}
}

func TestCollectSkipsUnrelatedCaches(t *testing.T) {
	tests := []struct {
		name       string
		pod        *corev1.Pod
		containers map[string]*nvidia.ContainerUsage
	}{
		{
			name: "pod is not GPU sharing",
			pod: func() *corev1.Pod {
				p := gpuSharingPod("uid-1", "trainer")
				p.Annotations = nil
				return p
			}(),
			containers: map[string]*nvidia.ContainerUsage{
				"uid-1_trainer": {PodUID: "uid-1", ContainerName: "trainer", Info: &fakeUsage{devices: 1, uuid: validUUID}},
			},
		},
		{
			name: "cache belongs to another pod",
			pod:  gpuSharingPod("uid-1", "trainer"),
			containers: map[string]*nvidia.ContainerUsage{
				"uid-2_trainer": {PodUID: "uid-2", ContainerName: "trainer", Info: &fakeUsage{devices: 1, uuid: validUUID}},
			},
		},
		{
			name: "cache belongs to a container not in the pod spec",
			pod:  gpuSharingPod("uid-1", "trainer"),
			containers: map[string]*nvidia.ContainerUsage{
				"uid-1_sidecar": {PodUID: "uid-1", ContainerName: "sidecar", Info: &fakeUsage{devices: 1, uuid: validUUID}},
			},
		},
		{
			name: "cache is not mapped yet",
			pod:  gpuSharingPod("uid-1", "trainer"),
			containers: map[string]*nvidia.ContainerUsage{
				"uid-1_trainer": {PodUID: "uid-1", ContainerName: "trainer"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			NewClusterManager("vGPU", reg, &fakeSource{containers: tt.containers}, podLister(t, tt.pod), false)

			families := gather(t, reg)
			for _, name := range containerMetricNames {
				if samples, ok := families[name]; ok {
					t.Errorf("metric %s was collected with %d samples, want none", name, len(samples))
				}
			}
		})
	}
}

func TestCollectLegacyMetricNames(t *testing.T) {
	source := &fakeSource{containers: map[string]*nvidia.ContainerUsage{
		"uid-1_trainer": {PodUID: "uid-1", ContainerName: "trainer", Info: &fakeUsage{
			devices: 1, uuid: validUUID, total: 1 << 30,
		}},
	}}
	reg := prometheus.NewRegistry()
	NewClusterManager("vGPU", reg, source, podLister(t, gpuSharingPod("uid-1", "trainer")), true)

	families := gather(t, reg)
	legacy := requireSingleSample(t, families, "vGPU_device_memory_usage_in_bytes")
	if legacy.value != float64(1<<30) {
		t.Errorf("legacy usage = %v, want %v", legacy.value, float64(1<<30))
	}
	if legacy.labels["ctrname"] != "trainer" {
		t.Errorf("legacy ctrname = %q, want %q", legacy.labels["ctrname"], "trainer")
	}
	// hami_* names stay alongside the legacy ones.
	requireSingleSample(t, families, "hami_vgpu_memory_used_bytes")
}

func gpuSharingPod(uid, containerName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "job-0",
			Namespace:   "team-a",
			UID:         types.UID(uid),
			Annotations: map[string]string{nvidia.GPUMemoryAnnotation: "4096"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: containerName}},
		},
	}
}

func podLister(t *testing.T, pods ...*corev1.Pod) listerscorev1.PodLister {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, pod := range pods {
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("failed to seed pod indexer: %v", err)
		}
	}
	return listerscorev1.NewPodLister(indexer)
}

type sample struct {
	labels map[string]string
	value  float64
}

// gather scrapes reg and flattens gauges by metric name. Host GPU metrics are absent
// because NVML is unavailable in tests, which Collect logs and tolerates.
func gather(t *testing.T, reg *prometheus.Registry) map[string][]sample {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	out := make(map[string][]sample, len(families))
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			labels := make(map[string]string, len(metric.GetLabel()))
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			out[family.GetName()] = append(out[family.GetName()], sample{labels: labels, value: gaugeValue(metric)})
		}
	}
	return out
}

func gaugeValue(metric *dto.Metric) float64 {
	if metric.GetGauge() != nil {
		return metric.GetGauge().GetValue()
	}
	return metric.GetUntyped().GetValue()
}

func requireSingleSample(t *testing.T, families map[string][]sample, name string) sample {
	t.Helper()
	samples, ok := families[name]
	if !ok {
		t.Fatalf("metric %s was not collected", name)
	}
	if len(samples) != 1 {
		t.Fatalf("metric %s has %d samples, want 1", name, len(samples))
	}
	return samples[0]
}
