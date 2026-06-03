# kai-resource-isolator

Syncs **libvgpu** onto GPU nodes via a **DaemonSet** and uses a **mutating admission webhook** to inject mounts and `ld.so.preload` into Pods that request HAMi vGPU-related resources. This aligns with the typical HAMi device-plugin layout using **hostPath `/usr/local/vgpu`**.

## Quick Start (OCI)

No need to clone the repo — install directly from the OCI registry:

```bash
helm install kai-resource-isolator oci://docker.io/projecthami/kai-resource-isolator \
  --namespace kai-resource-isolator --create-namespace \
  --version <version>-chart
```

Chart versions carry a `-chart` suffix (e.g. `1.0.0-chart`). Available versions are listed at [projecthami/kai-resource-isolator](https://hub.docker.com/r/projecthami/kai-resource-isolator/tags) on Docker Hub.

## Build container image

The build context must be the **`kai-resource-isolator` repository root** (the directory that contains `go.mod`, `libvgpu/`, and `cmd/`).

```bash
git submodule update --init --recursive
docker build -f docker/Dockerfile -t <registry>/<project>/kai-resource-isolator:<tag> .
```

### Customization

Tune `paths.containerVgpuMount` and `webhook.gpuShareResources` for your environment and HAMi extended resource names.

#### TLS

Because this chart installs a `MutatingWebhookConfiguration`, the webhook server requires a valid TLS certificate. The chart ships with two modes:

| Mode | Values | Requires |
|---|---|---|
| Helm hook (default) | `tls.patch.enabled: true` | Nothing — a Job auto-generates a self-signed cert and patches the webhook CA bundle |
| cert-manager | `tls.certManager.enabled: true` + `tls.patch.enabled: false` | [cert-manager](https://cert-manager.io/) installed in the cluster |

If your cluster already has cert-manager, switch to the cert-manager mode:

```bash
helm install kai-resource-isolator oci://docker.io/projecthami/kai-resource-isolator \
  --namespace kai-resource-isolator --create-namespace \
  --version <version>-chart \
  --set tls.certManager.enabled=true \
  --set tls.patch.enabled=false
```

After install, verify with `kubectl get daemonset`, `kubectl get mutatingwebhookconfiguration`, etc. Disable injection per Pod with annotation `kai-resource-isolator.io/inject: "false"`, or skip the webhook for a namespace with label `kai-resource-isolator.io/webhook=ignore`.

## Components (summary)

| Component | Role |
|---|---|
| DaemonSet (libsync) | Copies `libvgpu.so` to the configured path on each node |
| Mutating webhook | Injects volumes, `ld.so.preload` for Pods requesting GPU-sharing resources |

## Release process

On Git tag push (`v*`), the CI workflow:

1. Builds the Docker image (`linux/amd64` + `linux/arm64`) and pushes to Docker Hub as `projecthami/kai-resource-isolator:v<x.y.z>` and `:latest`
2. Packages the Helm chart (version `<x.y.z>-chart`) and pushes it as an OCI artifact to `oci://docker.io/projecthami/kai-resource-isolator`

For post-install hints, see the Helm-rendered **NOTES** printed after `helm install`.
