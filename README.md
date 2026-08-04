# kai-resource-isolator

`kai-resource-isolator` works alongside [KAI-Scheduler](https://github.com/NVIDIA/KAI-Scheduler) to enforce **GPU memory isolation** for GPU-sharing workloads. It leverages [HAMi-core](https://github.com/Project-HAMi/HAMi-core) to intercept CUDA calls inside the container and apply a hard memory limit, so each container only sees the GPU memory it was allocated.

For architecture details see the [Design](#design) section.

## Quick Start

### 1. Deploy KAI-Scheduler with GPU sharing enabled

Follow the [KAI-Scheduler deployment guide](https://github.com/NVIDIA/KAI-Scheduler/blob/main/docs/gpu-sharing/gpu-sharing.md) and enable `gpushare` and `hamicore`:

```bash
helm install kai-scheduler oci://ghcr.io/nvidia/kai-scheduler \
  --set global.gpuSharing=true \
  --set binder.plugins.hamicore.enabled=true \
  --namespace kai-scheduler --create-namespace
```

### 2. Deploy kai-resource-isolator

Install directly from the OCI registry:

```bash
helm upgrade --install kai-resource-isolator oci://docker.io/projecthami/kai-resource-isolator \
  --namespace kai-resource-isolator --create-namespace \
  --set monitor.enabled=true \
  --set monitor.serviceMonitor.enabled=true \
  --version 1.1.0-chart
```

The default `monitor.nodeSelector` is `nvidia.com/gpu.present: "true"` (NVIDIA GPU feature discovery). Set `monitor.runtimeClassName=nvidia` if NVML is only available through the NVIDIA runtime handler in your cluster.

Note: Chart versions carry a `-chart` suffix (e.g. `1.1.0-chart`). Available versions are listed at [projecthami/kai-resource-isolator](https://hub.docker.com/r/projecthami/kai-resource-isolator/tags) on Docker Hub.

## Build

The build context must be the **`kai-resource-isolator` repository root** (the directory that contains `go.mod`, `libvgpu/`, and `cmd/`).

```bash
git submodule update --init --recursive
docker build -f docker/Dockerfile -t <registry>/<project>/kai-resource-isolator:<tag> .
```

## Per-container VRAM metrics (optional)

`kai-vgpu-monitor` is a DaemonSet that reads the shared-memory cache `libvgpu.so` writes for each GPU container and exposes HAMi-compatible gauges (`hami_vgpu_memory_used_bytes`, `hami_vgpu_memory_limit_bytes`, `hami_container_device_utilization_ratio`, …) by using

```
curl {pod ip}:9394/metrics
```

## Customization

Tune `paths.containerVgpuMount` and `webhook.gpuShareResources` for your environment and HAMi extended resource names.

## Design

GPU sharing in KAI-Scheduler allows a Pod to request a fraction of a GPU (e.g. `0.5`) or a specific amount of GPU memory. Without memory isolation, however, containers could still access the full GPU memory at the CUDA level.

`kai-resource-isolator` closes this gap by combining two components:

| Component | Role |
|---|---|
| DaemonSet (libsync) | Copies `libvgpu.so` (HAMi-core) to `/usr/local/vgpu` on every GPU node |
| Mutating webhook | Injects the `libvgpu` hostPath volume and `ld.so.preload` into Pods that request GPU-sharing resources |

The full flow when a GPU-sharing Pod is submitted:

1. **KAI-Scheduler** selects a node and injects the `CUDA_DEVICE_MEMORY_LIMIT` environment variable into the Pod, set to the allocated memory amount.
2. **kai-resource-isolator webhook** injects a `hostPath` volume mount (`/usr/local/vgpu`) and patches `/etc/ld.so.preload` so that `libvgpu.so` is loaded by the container at runtime.
3. The container starts; `libvgpu.so` intercepts CUDA memory allocation calls and enforces the limit set by `CUDA_DEVICE_MEMORY_LIMIT`.

![Architecture](https://github.com/user-attachments/assets/ac7566fe-f79c-45fc-b3a1-24bc18ea6bc9)
