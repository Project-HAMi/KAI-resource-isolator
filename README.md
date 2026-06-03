# kai-resource-isolator

Syncs **libvgpu** onto GPU nodes via a **DaemonSet** and uses a **mutating admission webhook** to inject mounts and `ld.so.preload` into Pods that request HAMi vGPU-related resources. This aligns with the typical HAMi device-plugin layout using **hostPath `/usr/local/vgpu`**.

## Quick Start (OCI)

No need to clone the repo — install directly from the OCI registry:

```bash
helm install kai-resource-isolator oci://docker.io/projecthami/kai-resource-isolator-chart \
  --namespace kai-resource-isolator --create-namespace \
  --version <version>
```

Available chart versions are listed at [projecthami/kai-resource-isolator-chart](https://hub.docker.com/r/projecthami/kai-resource-isolator-chart/tags) on Docker Hub.

## Prerequisites (for building from source)

- **Binary build**: Go 1.25 or newer (match the version in `go.mod`).
- **Image build**: Docker or a compatible builder; the webhook image build must reach a Go module proxy (override via `GOPROXY` in `docker/Dockerfile` if needed).
- **Deployment**: A Kubernetes cluster and `kubectl`; **Helm 3.8+** is recommended (for OCI support).
- **Library source**: This repo uses `HAMi-core` as a git submodule at `libvgpu/` and builds `libvgpu.so` from source during `docker/Dockerfile` build.

## Build from source

Build the webhook binary locally:

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/webhook ./cmd/webhook
```

## Build container image

The build context must be the **`kai-resource-isolator` repository root** (the directory that contains `go.mod`, `libvgpu/`, and `cmd/`).

```bash
git submodule update --init --recursive
docker build -f docker/Dockerfile -t <registry>/<project>/kai-resource-isolator:<tag> .
```

## Deploy with Helm (from local chart)

Chart path: `chart/kai-resource-isolator`.

```bash
helm upgrade --install kai-resource-isolator ./chart/kai-resource-isolator \
  --namespace kai-resource-isolator --create-namespace \
  --set image.repository=<registry>/<project>/kai-resource-isolator \
  --set image.tag=<tag>
```

### TLS configuration

| Mode | Setting | Requires |
|---|---|---|
| Helm hook (default) | `tls.patch.enabled: true` | Nothing — Jobs auto-create and patch TLS |
| cert-manager | `tls.certManager.enabled: true` + `tls.patch.enabled: false` | [cert-manager](https://cert-manager.io/) installed |

### Customization

Tune `paths.containerVgpuMount` and `webhook.gpuShareResources` for your environment and HAMi extended resource names.

After install, verify with `kubectl get daemonset`, `kubectl get mutatingwebhookconfiguration`, etc. Disable injection per Pod with annotation `kai-resource-isolator.io/inject: "false"`, or skip the webhook for a namespace with label `kai-resource-isolator.io/webhook=ignore`.

## Components (summary)

| Component | Role |
|---|---|
| DaemonSet (libsync) | Copies `libvgpu.so` to the configured path on each node |
| Mutating webhook | Injects volumes, `ld.so.preload` for Pods requesting GPU-sharing resources |

## Release process

On Git tag push (`v*`), the CI workflow:

1. Builds the Docker image (`linux/amd64` + `linux/arm64`) and pushes to Docker Hub as `projecthami/kai-resource-isolator:v<x.y.z>` and `:latest`
2. Packages the Helm chart (version synced to tag) and pushes it as an OCI artifact to `oci://docker.io/projecthami/kai-resource-isolator-chart`

For post-install hints, see the Helm-rendered **NOTES** printed after `helm install`.
