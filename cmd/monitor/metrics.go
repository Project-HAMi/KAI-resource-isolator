/*
Copyright 2024 The HAMi Authors.
Copyright The HAMi Authors (KAI adaptations).
SPDX-License-Identifier: Apache-2.0

Ported from Project-HAMi/HAMi cmd/vGPUmonitor/metrics.go for kai-vgpu-monitor.
MIG metrics and feedback are omitted. Pod filter uses KAI gpu-fraction/gpu-memory
annotations on pods already scoped to this node.
*/

package main

import (
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/Project-HAMi/kai-resource-isolator/pkg/monitor/nvidia"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	listerscorev1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

// containerSource supplies the per-container libvgpu caches for a scrape.
// nvidia.ContainerLister is the production implementation; tests use a fake.
type containerSource interface {
	Update() error
	WithContainers(fn func(containers map[string]*nvidia.ContainerUsage))
	NodeName() string
}

// ClusterManager holds shared state for metrics collection.
// PodLister is node-scoped (same informer as ContainerLister); do not replace
// it with a cluster-wide lister or scrapes will include pods from other nodes.
type ClusterManager struct {
	Zone          string
	PodLister     listerscorev1.PodLister
	containers    containerSource
	LegacyMetrics bool
}

// ClusterManagerCollector implements prometheus.Collector for host NVML gauges
// and per-container hami_* gauges from libvgpu shared-memory caches.
type ClusterManagerCollector struct {
	ClusterManager *ClusterManager
}

var (
	hostGPUdesc = prometheus.NewDesc(
		"hami_host_gpu_memory_used_bytes",
		"GPU device memory usage in bytes",
		[]string{"device_index", "device_uuid", "device_type"}, nil,
	)

	hostGPUUtilizationdesc = prometheus.NewDesc(
		"hami_host_gpu_utilization_ratio",
		"GPU core utilization ratio (0-100)",
		[]string{"device_index", "device_uuid", "device_type"}, nil,
	)

	ctrvGPUdesc = prometheus.NewDesc(
		"hami_vgpu_memory_used_bytes",
		"vGPU device memory usage in bytes",
		[]string{"namespace", "pod", "container", "vdevice_index", "device_uuid"}, nil,
	)

	ctrvGPUlimitdesc = prometheus.NewDesc(
		"hami_vgpu_memory_limit_bytes",
		"vGPU device memory limit in bytes",
		[]string{"namespace", "pod", "container", "vdevice_index", "device_uuid"}, nil,
	)
	ctrDeviceMemorydesc = prometheus.NewDesc(
		"hami_container_device_memory_bytes",
		`Container device memory usage breakdown in bytes (The label "context_size", "module_size", "buffer_size" and "offset" will be deprecated in v2.10.0, use hami_vgpu_memory_context_bytes, hami_vgpu_memory_module_bytes and hami_vgpu_memory_buffer_bytes instead)`,
		[]string{"namespace", "pod", "container", "vdevice_index", "device_uuid", "context_size", "module_size", "buffer_size", "offset"}, nil,
	)
	ctrDeviceUtilizationdesc = prometheus.NewDesc(
		"hami_container_device_utilization_ratio",
		"Container device SM utilization ratio",
		[]string{"namespace", "pod", "container", "vdevice_index", "device_uuid"}, nil,
	)
	ctrDeviceLastKernelDesc = prometheus.NewDesc(
		"hami_container_last_kernel_elapsed_seconds",
		"Seconds since last kernel execution in container",
		[]string{"namespace", "pod", "container", "vdevice_index", "device_uuid"}, nil,
	)
	ctrDeviceMemoryContextDesc = prometheus.NewDesc(
		"hami_vgpu_memory_context_bytes",
		"Container device memory context size in bytes",
		[]string{"namespace", "pod", "container", "vdevice_index", "device_uuid"}, nil,
	)

	ctrDeviceMemoryModuleDesc = prometheus.NewDesc(
		"hami_vgpu_memory_module_bytes",
		"Container device memory module size in bytes",
		[]string{"namespace", "pod", "container", "vdevice_index", "device_uuid"}, nil,
	)

	ctrDeviceMemoryBufferDesc = prometheus.NewDesc(
		"hami_vgpu_memory_buffer_bytes",
		"Container device memory buffer size in bytes",
		[]string{"namespace", "pod", "container", "vdevice_index", "device_uuid"}, nil,
	)
)

var (
	legacyHostGPUdesc              *prometheus.Desc
	legacyHostGPUUtilizationdesc   *prometheus.Desc
	legacyCtrvGPUdesc              *prometheus.Desc
	legacyCtrvGPUlimitdesc         *prometheus.Desc
	legacyCtrDeviceMemorydesc      *prometheus.Desc
	legacyCtrDeviceUtilizationdesc *prometheus.Desc
	legacyCtrDeviceLastKernelDesc  *prometheus.Desc
)

// initLegacyDescriptors builds the old HAMi metric names (HostGPUMemoryUsage, etc.).
// Only used when --legacy-metrics is set; leave off unless an old dashboard needs them.
func initLegacyDescriptors() {
	legacyHostGPUdesc = prometheus.NewDesc(
		"HostGPUMemoryUsage",
		"GPU device memory usage",
		[]string{"deviceidx", "deviceuuid", "devicetype"}, nil,
	)
	legacyHostGPUUtilizationdesc = prometheus.NewDesc(
		"HostCoreUtilization",
		"GPU core utilization",
		[]string{"deviceidx", "deviceuuid", "devicetype"}, nil,
	)
	legacyCtrvGPUdesc = prometheus.NewDesc(
		"vGPU_device_memory_usage_in_bytes",
		"vGPU device usage",
		[]string{"podnamespace", "podname", "ctrname", "vdeviceid", "deviceuuid"}, nil,
	)
	legacyCtrvGPUlimitdesc = prometheus.NewDesc(
		"vGPU_device_memory_limit_in_bytes",
		"vGPU device limit",
		[]string{"podnamespace", "podname", "ctrname", "vdeviceid", "deviceuuid"}, nil,
	)
	legacyCtrDeviceMemorydesc = prometheus.NewDesc(
		"Device_memory_desc_of_container",
		"Container device memory description",
		[]string{"podnamespace", "podname", "ctrname", "vdeviceid", "deviceuuid", "context", "module", "data", "offset"}, nil,
	)
	legacyCtrDeviceUtilizationdesc = prometheus.NewDesc(
		"Device_utilization_desc_of_container",
		"Container device utilization description",
		[]string{"podnamespace", "podname", "ctrname", "vdeviceid", "deviceuuid"}, nil,
	)
	legacyCtrDeviceLastKernelDesc = prometheus.NewDesc(
		"Device_last_kernel_of_container",
		"Container device last kernel description",
		[]string{"podnamespace", "podname", "ctrname", "vdeviceid", "deviceuuid"}, nil,
	)
}

// sendLegacyMetric emits a legacy-named series when --legacy-metrics is enabled.
// Nil desc is a no-op (legacy descriptors unset).
func sendLegacyMetric(ch chan<- prometheus.Metric, desc *prometheus.Desc, valueType prometheus.ValueType, value float64, labels ...string) {
	if desc == nil {
		return
	}
	if err := sendMetric(ch, desc, valueType, value, labels...); err != nil {
		klog.V(4).Infof("Failed to send legacy metric: %v", err)
	}
}

// Describe implements prometheus.Collector (static descriptor set).
func (cc ClusterManagerCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- hostGPUdesc
	ch <- ctrvGPUdesc
	ch <- ctrvGPUlimitdesc
	ch <- hostGPUUtilizationdesc
	ch <- ctrDeviceMemorydesc
	ch <- ctrDeviceUtilizationdesc
	ch <- ctrDeviceLastKernelDesc
	ch <- ctrDeviceMemoryContextDesc
	ch <- ctrDeviceMemoryModuleDesc
	ch <- ctrDeviceMemoryBufferDesc

	if cc.ClusterManager.LegacyMetrics {
		ch <- legacyHostGPUdesc
		ch <- legacyHostGPUUtilizationdesc
		ch <- legacyCtrvGPUdesc
		ch <- legacyCtrvGPUlimitdesc
		ch <- legacyCtrDeviceMemorydesc
		ch <- legacyCtrDeviceUtilizationdesc
		ch <- legacyCtrDeviceLastKernelDesc
	}
}

// Collect implements prometheus.Collector.
//
// Pay attention:
//   - Calls Update() so the mmap map is refreshed on every scrape.
//   - NVML init failure only drops host GPU metrics; container metrics still run.
//   - Container metrics appear only after libvgpu has written a .cache (after cuInit).
func (cc ClusterManagerCollector) Collect(ch chan<- prometheus.Metric) {
	klog.Info("Starting to collect metrics for kai-vgpu-monitor")

	if err := cc.ClusterManager.containers.Update(); err != nil {
		klog.Errorf("Failed to update container lister: %v", err)
	}

	if err := cc.collectGPUInfo(ch); err != nil {
		klog.Errorf("Failed to collect GPU info: %v", err)
	}

	if err := cc.collectPodAndContainerInfo(ch); err != nil {
		klog.Errorf("Failed to collect Pod and Container info: %v", err)
	}

	klog.Info("Finished collecting metrics for kai-vgpu-monitor")
}

// collectGPUInfo publishes host-wide NVML memory/utilization gauges for each GPU.
// Requires NVIDIA drivers visible in the pod; per-device errors are logged and skipped.
func (cc ClusterManagerCollector) collectGPUInfo(ch chan<- prometheus.Metric) error {
	if err := cc.initNVML(); err != nil {
		return err
	}
	defer func() {
		if ret := nvml.Shutdown(); ret != nvml.SUCCESS {
			klog.Errorf("nvml Shutdown err: %s", nvml.ErrorString(ret))
		}
	}()

	devnum, err := cc.getDeviceCount()
	if err != nil {
		return err
	}

	for ii := range devnum {
		if err := cc.collectGPUDeviceMetrics(ch, ii); err != nil {
			klog.Error("Failed to collect metrics for GPU device ", ii, ": ", err)
		}
	}

	return nil
}

// initNVML initializes the NVML library for this scrape. Pair with nvml.Shutdown.
func (cc ClusterManagerCollector) initNVML() error {
	nvret := nvml.Init()
	if nvret != nvml.SUCCESS {
		return fmt.Errorf("nvml Init err: %s", nvml.ErrorString(nvret))
	}
	return nil
}

// getDeviceCount returns the number of NVML-visible GPUs on this node.
func (cc ClusterManagerCollector) getDeviceCount() (int, error) {
	devnum, nvret := nvml.DeviceGetCount()
	if nvret != nvml.SUCCESS {
		return 0, fmt.Errorf("nvml GetDeviceCount err: %s", nvml.ErrorString(nvret))
	}
	return devnum, nil
}

// collectGPUDeviceMetrics emits host memory + utilization metrics for one GPU index.
func (cc ClusterManagerCollector) collectGPUDeviceMetrics(ch chan<- prometheus.Metric, index int) error {
	hdev, nvret := nvml.DeviceGetHandleByIndex(index)
	if nvret != nvml.SUCCESS {
		return fmt.Errorf("nvml DeviceGetHandleByIndex err: %s", nvml.ErrorString(nvret))
	}

	if err := cc.collectGPUMemoryMetrics(ch, hdev, index); err != nil {
		return err
	}

	return cc.collectGPUUtilizationMetrics(ch, hdev, index)
}

// collectGPUMemoryMetrics emits hami_host_gpu_memory_used_bytes for one device.
// ERROR_NOT_SUPPORTED (e.g. some unified-memory GPUs) is treated as success/skip.
func (cc ClusterManagerCollector) collectGPUMemoryMetrics(ch chan<- prometheus.Metric, hdev nvml.Device, index int) error {
	memory, ret := hdev.GetMemoryInfo()
	if ret == nvml.ERROR_NOT_SUPPORTED {
		klog.V(3).Infof("Memory metrics not supported for device %d (unified memory architecture), skipping", index)
		return nil
	}
	if ret != nvml.SUCCESS {
		return fmt.Errorf("nvml get memory error ret=%d", ret)
	}

	uuid, nvret := hdev.GetUUID()
	if nvret != nvml.SUCCESS {
		return fmt.Errorf("nvml GetUUID err: %s", nvml.ErrorString(nvret))
	}

	deviceName, nvret := hdev.GetName()
	if nvret != nvml.SUCCESS {
		return fmt.Errorf("nvml GetName err: %s", nvml.ErrorString(nvret))
	}

	deviceName = "NVIDIA-" + deviceName

	ch <- prometheus.MustNewConstMetric(
		hostGPUdesc,
		prometheus.GaugeValue,
		float64(memory.Used),
		fmt.Sprint(index), uuid, deviceName,
	)

	sendLegacyMetric(ch, legacyHostGPUdesc, prometheus.GaugeValue, float64(memory.Used),
		fmt.Sprint(index), uuid, deviceName,
	)

	return nil
}

// collectGPUUtilizationMetrics emits hami_host_gpu_utilization_ratio (0–100) for one device.
func (cc ClusterManagerCollector) collectGPUUtilizationMetrics(ch chan<- prometheus.Metric, hdev nvml.Device, index int) error {
	util, nvret := hdev.GetUtilizationRates()
	if nvret != nvml.SUCCESS {
		return fmt.Errorf("nvml GetUtilizationRates err: %s", nvml.ErrorString(nvret))
	}

	uuid, nvret := hdev.GetUUID()
	if nvret != nvml.SUCCESS {
		return fmt.Errorf("nvml GetUUID err: %s", nvml.ErrorString(nvret))
	}

	deviceName, nvret := hdev.GetName()
	if nvret != nvml.SUCCESS {
		return fmt.Errorf("nvml GetName err: %s", nvml.ErrorString(nvret))
	}

	deviceName = "NVIDIA-" + deviceName

	ch <- prometheus.MustNewConstMetric(
		hostGPUUtilizationdesc,
		prometheus.GaugeValue,
		float64(util.Gpu),
		fmt.Sprint(index), uuid, deviceName,
	)

	sendLegacyMetric(ch, legacyHostGPUUtilizationdesc, prometheus.GaugeValue, float64(util.Gpu),
		fmt.Sprint(index), uuid, deviceName,
	)

	return nil
}

// collectPodAndContainerInfo joins node pods that have KAI GPU-sharing annotations
// with mmap'd caches keyed by pod UID, then emits per-container hami_* gauges.
//
// Pay attention:
//   - Filter is annotation-based (gpu-fraction / gpu-memory), not hami.io/vgpu-node.
//   - Pods without a matching cache dir are skipped (webhook mount / cuInit not ready).
//   - Only regular Spec.Containers are matched; init-container GPU is out of scope for v1.
//   - Caches are read inside WithContainers: the values overlay mmap'd memory that a
//     concurrent Update() may unmap, so nothing may escape the callback.
func (cc ClusterManagerCollector) collectPodAndContainerInfo(ch chan<- prometheus.Metric) error {
	pods, err := cc.ClusterManager.PodLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list pods for node %s: %v", cc.ClusterManager.containers.NodeName(), err)
		return fmt.Errorf("failed to list pods: %w", err)
	}

	nowSec := time.Now().Unix()
	matchedPods := 0

	cc.ClusterManager.containers.WithContainers(func(containers map[string]*nvidia.ContainerUsage) {
		containerMap := make(map[string][]*nvidia.ContainerUsage, len(containers))
		for _, c := range containers {
			if c.Info != nil && c.PodUID != "" {
				containerMap[c.PodUID] = append(containerMap[c.PodUID], c)
			}
		}

		for _, pod := range pods {
			if !nvidia.IsGPUSharingPod(pod) {
				continue
			}
			matchedPods++

			podContainers, found := containerMap[string(pod.UID)]
			if !found {
				klog.V(5).Infof("No containers found for pod %s/%s", pod.Namespace, pod.Name)
				continue
			}

			klog.V(5).Infof("Processing Pod %s/%s", pod.Namespace, pod.Name)

			for _, ctr := range pod.Spec.Containers {
				for _, c := range podContainers {
					if c.ContainerName == ctr.Name {
						klog.V(5).Infof("Processing Container %s in Pod %s/%s", ctr.Name, pod.Namespace, pod.Name)
						if err := cc.collectContainerMetrics(ch, pod, ctr, c, nowSec); err != nil {
							klog.Errorf("Failed to collect metrics for container %s in Pod %s/%s: %v", ctr.Name, pod.Namespace, pod.Name, err)
						}
						break
					}
				}
			}
		}
	})

	klog.V(4).Infof("Finished collecting metrics for %d GPU-sharing pods (of %d on node)", matchedPods, len(pods))
	return nil
}

// collectContainerMetrics emits all per-vdevice hami_* gauges for one container cache.
//
// Pay attention:
//   - UUID is truncated to 40 chars (libvgpu pads a 96-byte buffer).
//   - Invalid UTF-8 UUID means shm not initialized yet; skip device, do not fail the scrape.
//   - last-kernel metric is omitted when LastKernelTime() is 0 (v0 caches always return 0).
func (cc ClusterManagerCollector) collectContainerMetrics(ch chan<- prometheus.Metric, pod *corev1.Pod, ctr corev1.Container, c *nvidia.ContainerUsage, nowSec int64) error {
	if c == nil || c.Info == nil {
		klog.Errorf("Container or ContainerInfo is nil for Pod %s/%s, Container %s", pod.Namespace, pod.Name, ctr.Name)
		return fmt.Errorf("container or container info is nil")
	}

	for i := range c.Info.DeviceNum() {
		if !c.Info.IsValidUUID(i) {
			klog.V(5).Infof("Device %d in Pod %s/%s, Container %s has no UUID yet; skipping until next scrape", i, pod.Namespace, pod.Name, ctr.Name)
			continue
		}
		uuid := c.Info.DeviceUUID(i)
		if len(uuid) > 40 {
			uuid = uuid[:40]
		}
		if !utf8.ValidString(uuid) {
			klog.Warningf("Device %d in Pod %s/%s, Container %s has invalid UTF-8 UUID (shared memory not yet initialised); skipping until next scrape", i, pod.Namespace, pod.Name, ctr.Name)
			continue
		}

		memoryTotal := c.Info.DeviceMemoryTotal(i)
		memoryLimit := c.Info.DeviceMemoryLimit(i)
		memoryContextSize := c.Info.DeviceMemoryContextSize(i)
		memoryModuleSize := c.Info.DeviceMemoryModuleSize(i)
		memoryBufferSize := c.Info.DeviceMemoryBufferSize(i)
		smUtil := c.Info.DeviceSmUtil(i)
		lastKernelTime := c.Info.LastKernelTime()

		metricLabels := []string{pod.Namespace, pod.Name, ctr.Name, fmt.Sprint(i), uuid}

		if err := sendMetric(ch, ctrvGPUdesc, prometheus.GaugeValue, float64(memoryTotal), metricLabels...); err != nil {
			klog.Errorf("Failed to send memoryTotal metric: %v", err)
			return err
		}
		sendLegacyMetric(ch, legacyCtrvGPUdesc, prometheus.GaugeValue, float64(memoryTotal), metricLabels...)

		if err := sendMetric(ch, ctrvGPUlimitdesc, prometheus.GaugeValue, float64(memoryLimit), metricLabels...); err != nil {
			klog.Errorf("Failed to send memoryLimit metric: %v", err)
			return err
		}
		sendLegacyMetric(ch, legacyCtrvGPUlimitdesc, prometheus.GaugeValue, float64(memoryLimit), metricLabels...)

		memoryOffset := memoryTotal - memoryContextSize - memoryModuleSize - memoryBufferSize
		memoryLabels := append(metricLabels, fmt.Sprint(memoryContextSize), fmt.Sprint(memoryModuleSize), fmt.Sprint(memoryBufferSize), fmt.Sprint(memoryOffset))
		if err := sendMetric(ch, ctrDeviceMemorydesc, prometheus.GaugeValue, float64(memoryTotal), memoryLabels...); err != nil {
			klog.Errorf("Failed to send device memory desc: %v", err)
			return err
		}
		sendLegacyMetric(ch, legacyCtrDeviceMemorydesc, prometheus.GaugeValue, float64(memoryTotal), memoryLabels...)

		if err := sendMetric(ch, ctrDeviceUtilizationdesc, prometheus.GaugeValue, float64(smUtil), metricLabels...); err != nil {
			klog.Errorf("Failed to send device utilization desc: %v", err)
			return err
		}
		sendLegacyMetric(ch, legacyCtrDeviceUtilizationdesc, prometheus.GaugeValue, float64(smUtil), metricLabels...)

		if err := sendMetric(ch, ctrDeviceMemoryContextDesc, prometheus.GaugeValue, float64(memoryContextSize), metricLabels...); err != nil {
			klog.Errorf("Failed to send Device Memory context size metric: %v", err)
			return err
		}
		if err := sendMetric(ch, ctrDeviceMemoryModuleDesc, prometheus.GaugeValue, float64(memoryModuleSize), metricLabels...); err != nil {
			klog.Errorf("Failed to send Device Memory module size metric: %v", err)
			return err
		}
		if err := sendMetric(ch, ctrDeviceMemoryBufferDesc, prometheus.GaugeValue, float64(memoryBufferSize), metricLabels...); err != nil {
			klog.Errorf("Failed to send Device Memory buffer size metric: %v", err)
			return err
		}

		if lastKernelTime > 0 {
			lastSec := max(nowSec-lastKernelTime, 0)
			if err := sendMetric(ch, ctrDeviceLastKernelDesc, prometheus.GaugeValue, float64(lastSec), metricLabels...); err != nil {
				klog.Errorf("Failed to send last kernel time metric: %v", err)
				return err
			}
			sendLegacyMetric(ch, legacyCtrDeviceLastKernelDesc, prometheus.GaugeValue, float64(lastSec), metricLabels...)
		}
	}

	klog.V(5).Infof("Successfully collected metrics for Pod %s/%s, Container %s", pod.Namespace, pod.Name, ctr.Name)
	return nil
}

// sendMetric builds a ConstMetric and sends it on ch. Label count must match the Desc.
func sendMetric(ch chan<- prometheus.Metric, desc *prometheus.Desc, valueType prometheus.ValueType, value float64, labels ...string) error {
	metric, err := prometheus.NewConstMetric(desc, valueType, value, labels...)
	if err != nil {
		return fmt.Errorf("failed to create metric: %w", err)
	}
	ch <- metric
	return nil
}

// NewClusterManager registers ClusterManagerCollector on reg with const label zone.
//
// Pay attention: zone is "vGPU" to match HAMi dashboards; changing it breaks label
// selectors on existing Grafana boards. podLister must be node-scoped (pass the one
// owned by the container source) or scrapes report pods from other nodes.
func NewClusterManager(zone string, reg prometheus.Registerer, containers containerSource, podLister listerscorev1.PodLister, legacyMetrics bool) *ClusterManager {
	if legacyMetrics {
		initLegacyDescriptors()
	}
	c := &ClusterManager{
		Zone:          zone,
		PodLister:     podLister,
		containers:    containers,
		LegacyMetrics: legacyMetrics,
	}

	cc := ClusterManagerCollector{ClusterManager: c}
	prometheus.WrapRegistererWith(prometheus.Labels{"zone": zone}, reg).MustRegister(cc)
	return c
}
