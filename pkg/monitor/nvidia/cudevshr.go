/*
Copyright 2024 The HAMi Authors.
Copyright The HAMi Authors (KAI adaptations).
SPDX-License-Identifier: Apache-2.0

Ported from Project-HAMi/HAMi pkg/monitor/nvidia/cudevshr.go for kai-vgpu-monitor.
*/

package nvidia

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	v0 "github.com/Project-HAMi/kai-resource-isolator/pkg/monitor/nvidia/v0"
	v1 "github.com/Project-HAMi/kai-resource-isolator/pkg/monitor/nvidia/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

const (
	// SharedRegionMagicFlag is written by libvgpu into the cache header
	// (MULTIPROCESS_SHARED_REGION_MAGIC_FLAG). Mismatched magic means the
	// file is not a valid shared region (or not yet initialized).
	SharedRegionMagicFlag = 19920718

	// NodeNameEnvName is the env var set to the Kubernetes node name
	// (usually via fieldRef spec.nodeName on the DaemonSet).
	NodeNameEnvName = "NODE_NAME"

	// KAI GPU-sharing pod annotation keys (see KAI-Scheduler constants).
	// Presence of either marks the pod as in-scope for container metrics.
	GPUFractionAnnotation = "gpu-fraction"
	GPUMemoryAnnotation   = "gpu-memory"

	// legacyV0CacheSize is the exact file size written by pre-versioned libvgpu
	// (sizeof(shared_region_t) plus the trailing byte from lseek+write).
	legacyV0CacheSize = 1197897
)

// headerT is the leading bytes of a libvgpu cache (layout-compatible with C).
type headerT struct {
	initializedFlag int32
	majorVersion    int32
	minorVersion    int32
}

// UsageInfo reads libvgpu shared-memory cache fields.
// Implementations are the v0/v1 Spec overlays; keep struct layouts byte-identical
// to HAMi-core shared_region_t or mmap reads will be wrong.
type UsageInfo interface {
	DeviceMax() int
	DeviceNum() int
	DeviceMemoryContextSize(idx int) uint64
	DeviceMemoryModuleSize(idx int) uint64
	DeviceMemoryBufferSize(idx int) uint64
	DeviceMemoryOffset(idx int) uint64
	DeviceMemoryTotal(idx int) uint64
	DeviceSmUtil(idx int) uint64
	SetDeviceSmLimit(l uint64)
	IsValidUUID(idx int) bool
	DeviceUUID(idx int) string
	DeviceMemoryLimit(idx int) uint64
	SetDeviceMemoryLimit(l uint64)
	LastKernelTime() int64
	GetPriority() int
	GetRecentKernel() int32
	SetRecentKernel(v int32)
	GetUtilizationSwitch() int32
	SetUtilizationSwitch(v int32)
}

// ContainerUsage is one container's mmap'd libvgpu cache.
// data must be Munmap'd when the entry is removed; Info overlays data.
type ContainerUsage struct {
	PodUID        string
	ContainerName string
	data          []byte
	Info          UsageInfo
}

// ContainerLister tracks per-container caches under {mount}/containers/.
// Layout matches HAMi-core #219: one shared-region file per container at
// {mount}/containers/{podUID}_{containerName} (not a directory).
type ContainerLister struct {
	containerPath string
	containers    map[string]*ContainerUsage
	mutex         sync.Mutex
	clientset     *kubernetes.Clientset
	nodeName      string

	informerFactory informers.SharedInformerFactory
	podInformer     cache.SharedIndexInformer
	podLister       corelisters.PodLister
	podListerSynced cache.InformerSynced
	stopCh          chan struct{}
}

// resyncInterval is the pod-informer resync period and the grace window before
// deleting orphan cache files whose pod UID is gone. Override with HAMI_RESYNC_INTERVAL.
var resyncInterval = 5 * time.Minute

func init() {
	if os.Getenv("HAMI_RESYNC_INTERVAL") != "" {
		if interval, err := time.ParseDuration(os.Getenv("HAMI_RESYNC_INTERVAL")); err == nil {
			resyncInterval = interval
		} else {
			klog.Warningf("Invalid HAMI_RESYNC_INTERVAL value: %s, using default %v", os.Getenv("HAMI_RESYNC_INTERVAL"), resyncInterval)
		}
	}
}

// MountPath returns the vGPU mount root from CONTAINER_VGPU_MOUNT or HOOK_PATH.
//
// Pay attention: prefer CONTAINER_VGPU_MOUNT (KAI chart). HOOK_PATH is an HAMi
// alias. Value must match webhook injection and the DaemonSet hostPath
// (default /usr/local/vgpu); caches are read from {MountPath}/containers.
func MountPath() (string, error) {
	if v := os.Getenv("CONTAINER_VGPU_MOUNT"); v != "" {
		return v, nil
	}
	if v := os.Getenv("HOOK_PATH"); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("CONTAINER_VGPU_MOUNT or HOOK_PATH not set")
}

// IsGPUSharingPod reports whether the pod requests KAI GPU sharing via
// gpu-fraction or gpu-memory annotations (replaces HAMi's hami.io/vgpu-node filter).
func IsGPUSharingPod(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	if _, ok := pod.Annotations[GPUFractionAnnotation]; ok {
		return true
	}
	if _, ok := pod.Annotations[GPUMemoryAnnotation]; ok {
		return true
	}
	return false
}

// NewContainerLister builds a node-scoped lister that scans libvgpu cache files.
//
// Pay attention:
//   - Blocks until the pod informer syncs (needs RBAC pods get/list/watch).
//   - Empty KUBECONFIG uses in-cluster config.
//   - Call Stop() on shutdown; Stop closes the informer stopCh once only.
func NewContainerLister() (*ContainerLister, error) {
	mountPath, err := MountPath()
	if err != nil {
		return nil, err
	}
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		klog.Errorf("Failed to build kubeconfig: %v", err)
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Errorf("Failed to build clientset: %v", err)
		return nil, err
	}

	nodeName := os.Getenv(NodeNameEnvName)
	if nodeName == "" {
		return nil, fmt.Errorf("env %s not set", NodeNameEnvName)
	}

	lister := &ContainerLister{
		containerPath: filepath.Join(mountPath, "containers"),
		containers:    make(map[string]*ContainerUsage),
		clientset:     clientset,
		nodeName:      nodeName,
		stopCh:        make(chan struct{}),
	}

	if err := lister.initInformerWithConfig(resyncInterval); err != nil {
		return nil, err
	}

	return lister, nil
}

// WithContainers calls fn with the cache map while holding the lister lock,
// keyed by {podUID}_{containerName}.
//
// Pay attention: ContainerUsage.Info reads mmap'd memory that Update() unmaps when
// a pod goes away, so fn must read values it needs and must not retain the map or
// any *ContainerUsage after returning. fn must not call Update() (same mutex).
func (l *ContainerLister) WithContainers(fn func(containers map[string]*ContainerUsage)) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	fn(l.containers)
}

// Clientset returns the Kubernetes clientset used by the pod informer.
func (l *ContainerLister) Clientset() *kubernetes.Clientset {
	return l.clientset
}

// PodLister returns the node-scoped pod lister (spec.nodeName = NODE_NAME).
func (l *ContainerLister) PodLister() corelisters.PodLister {
	return l.podLister
}

// NodeName returns the node this lister is bound to.
func (l *ContainerLister) NodeName() string {
	return l.nodeName
}

// Stop stops the pod informer. Must not be called twice (closes stopCh).
func (l *ContainerLister) Stop() {
	close(l.stopCh)
}

// Update rescans host cache files, mmaps new ones, and deletes stale ones.
//
// Pay attention:
//   - Only regular files named {podUID}_{containerName} are considered (HAMi-core #219).
//     Directories are skipped; that is the HAMi device-plugin layout, not KAI's.
//   - Orphan files (pod UID gone) are removed after resyncInterval grace.
//   - Existing map entries are not reloaded; values update via shared mmap.
//   - Safe to call from the ticker and from Collect concurrently (mutex).
func (l *ContainerLister) Update() error {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	entries, err := os.ReadDir(l.containerPath)
	if err != nil {
		return err
	}

	pods, err := l.podLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list pods: %v", err)
	}

	podUIDs := make(map[string]bool, len(pods))
	for _, pod := range pods {
		podUIDs[string(pod.UID)] = true
	}

	for _, entry := range entries {
		if entry.IsDir() {
			klog.V(4).Infof("Skipping directory %q; KAI expects a flat cache file (HAMi-core #219)", entry.Name())
			continue
		}
		entryPath := filepath.Join(l.containerPath, entry.Name())
		podUID, containerName, ok := parseCacheFileName(entry.Name())
		if !ok {
			klog.V(4).Infof("Skipping cache file with unexpected name %q", entry.Name())
			continue
		}
		if !podUIDs[podUID] {
			entryInfo, err := os.Stat(entryPath)
			if err == nil && entryInfo.ModTime().Add(resyncInterval).After(time.Now()) {
				continue
			}
			klog.Infof("Removing %s in monitorpath, pod %s is gone", entryPath, podUID)
			l.dropContainer(entry.Name())
			_ = os.Remove(entryPath)
			continue
		}
		if _, ok := l.containers[entry.Name()]; ok {
			continue
		}
		usage, err := loadCache(entryPath)
		if err != nil {
			klog.Errorf("Failed to load cache: %s, error: %v", entryPath, err)
			continue
		}
		if usage == nil {
			// libvgpu has not initialised the shared region yet (no cuInit in container).
			continue
		}
		usage.PodUID = podUID
		usage.ContainerName = containerName
		l.containers[entry.Name()] = usage
		klog.Infof("Adding ctr cache %s in monitorpath", entryPath)
	}
	return nil
}

// dropContainer unmaps and forgets one cache entry. Callers must hold l.mutex.
func (l *ContainerLister) dropContainer(name string) {
	c, ok := l.containers[name]
	if !ok {
		return
	}
	delete(l.containers, name)
	if err := syscall.Munmap(c.data); err != nil {
		klog.Errorf("Failed to munmap cache of %s: %v", name, err)
	}
}

// parseCacheFileName splits "{podUID}_{containerName}" on the first underscore.
// Container names that contain "_" are preserved (SplitN 2). Returns ok=false
// if the name has no separator or empty parts.
func parseCacheFileName(name string) (podUID, containerName string, ok bool) {
	parts := strings.SplitN(name, "_", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// loadCache mmaps one shared-region file at {mount}/containers/{podUID}_{containerName}
// (HAMi-core #219) and selects the v0/v1 overlay.
//
// Pay attention:
//   - Returns (nil, nil) while libvgpu has created the file but not yet written the
//     magic (no cuInit yet); the caller retries on a later Update.
//   - Needs PROT_WRITE|MAP_SHARED and typically SYS_ADMIN/privileged for mmap.
//   - Size legacyV0CacheSize → v0, else majorVersion 1 → v1; the mapping must cover
//     the whole shared region before casting.
func loadCache(cacheFile string) (*ContainerUsage, error) {
	info, err := os.Stat(cacheFile)
	if err != nil {
		klog.Errorf("Failed to stat cache file: %s, error: %v", cacheFile, err)
		return nil, err
	}
	if info.Size() < int64(unsafe.Sizeof(headerT{})) {
		klog.V(4).Infof("Cache file %s is %d bytes, libvgpu has not sized it yet", cacheFile, info.Size())
		return nil, nil
	}
	f, err := os.OpenFile(cacheFile, os.O_RDWR, 0666)
	if err != nil {
		klog.Errorf("Failed to open cache file: %s, error: %v", cacheFile, err)
		return nil, err
	}
	defer func(f *os.File) {
		_ = f.Close()
	}(f)
	data, err := syscall.Mmap(int(f.Fd()), 0, int(info.Size()), syscall.PROT_WRITE|syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		klog.Errorf("Failed to mmap cache file: %s, error: %v", cacheFile, err)
		return nil, err
	}
	head := (*headerT)(unsafe.Pointer(&data[0]))
	if head.initializedFlag != SharedRegionMagicFlag {
		_ = syscall.Munmap(data)
		klog.V(4).Infof("Cache file %s has no magic flag yet, container has not called cuInit", cacheFile)
		return nil, nil
	}

	usage := &ContainerUsage{data: data}
	switch {
	case info.Size() == legacyV0CacheSize:
		if info.Size() < int64(v0.SpecSize()) {
			return nil, truncatedCache(data, cacheFile, info.Size(), v0.SpecSize())
		}
		klog.V(3).Infof("Casting cache file %s as v0", cacheFile)
		usage.Info = v0.CastSpec(data)
	case head.majorVersion == 1:
		if info.Size() < int64(v1.SpecSize()) {
			return nil, truncatedCache(data, cacheFile, info.Size(), v1.SpecSize())
		}
		klog.V(3).Infof("Casting cache file %s as v1", cacheFile)
		usage.Info = v1.CastSpec(data)
	default:
		_ = syscall.Munmap(data)
		return nil, fmt.Errorf("unknown cache file size %d version %d.%d", info.Size(), head.majorVersion, head.minorVersion)
	}
	return usage, nil
}

// truncatedCache unmaps data and reports a shared region too short to overlay.
func truncatedCache(data []byte, cacheFile string, size int64, want int) error {
	_ = syscall.Munmap(data)
	return fmt.Errorf("cache file %s is %d bytes, shared region needs %d", cacheFile, size, want)
}

// initInformerWithConfig starts a SharedInformerFactory filtered to this node.
// Blocks until the pod cache has synced.
func (l *ContainerLister) initInformerWithConfig(resyncInterval time.Duration) error {
	l.informerFactory = informers.NewSharedInformerFactoryWithOptions(
		l.clientset,
		resyncInterval,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fmt.Sprintf("spec.nodeName=%s", l.nodeName)
		}),
	)

	podInformer := l.informerFactory.Core().V1().Pods()
	l.podInformer = podInformer.Informer()
	l.podLister = podInformer.Lister()
	l.podListerSynced = l.podInformer.HasSynced

	l.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: l.onPodDelete,
	})

	l.informerFactory.Start(l.stopCh)

	if !cache.WaitForCacheSync(l.stopCh, l.podListerSynced) {
		return fmt.Errorf("failed to sync pod informer cache")
	}

	klog.Info("Pod informer started successfully")
	return nil
}

// onPodDelete logs pod removals. Cache-dir cleanup is handled lazily in Update()
// once the UID disappears from the lister (with resyncInterval grace).
func (l *ContainerLister) onPodDelete(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("couldn't get object from tombstone %+v", obj)
			return
		}
		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			klog.Errorf("tombstone contained object that is not a Pod: %+v", obj)
			return
		}
	}
	klog.V(5).Infof("Pod removed: %s/%s", pod.Namespace, pod.Name)
}
