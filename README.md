# kai-resource-isolator

Syncs **libvgpu** onto GPU nodes via a **DaemonSet** and uses a **mutating admission webhook** to inject mounts and `ld.so.preload` into Pods that request HAMi vGPU-related resources. This aligns with the typical HAMi device-plugin layout using **hostPath `/usr/local/vgpu`**.

## Prerequisites

- **Binary build**: Go 1.25 or newer (match the version in `go.mod`).
- **Image build**: Docker or a compatible builder; the webhook image build must reach a Go module proxy (override via `GOPROXY` in `docker/Dockerfile.webhook` if needed).
- **Deployment**: A Kubernetes cluster and `kubectl`; **Helm 3** is recommended.
- **Library image**: `docker/Dockerfile.libsync` expects a real **`libvgpu.so`** (matching your HAMi version) at the **repository root** before you build.

## Build locally (without Docker)

From this directory:

```bash
cd /path/to/kai-resource-isolator
go mod download
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/webhook ./cmd/webhook
```

To sanity-check compilation, you can also run `go test ./...` when tests exist.

## Build container images

Dockerfiles live under **`docker/`**. The build context must be the **`kai-resource-isolator` repository root** (the directory that contains `go.mod`, `libvgpu.so`, and `cmd/`).

### From this repository root

```bash
cd /path/to/kai-resource-isolator

docker build -f docker/Dockerfile.libsync -t <registry>/<project>/kai-resource-isolator-lib:<tag> .
docker build -f docker/Dockerfile.webhook -t <registry>/<project>/kai-resource-isolator-webhook:<tag> .
```

Replace `<registry>/<project>` and `<tag>` with your registry and image tag.

### When this repo is a subdirectory of a monorepo

If the component lives at `kai-resource-isolator/` under a parent repository, run from the **parent repository root**:

```bash
docker build -f kai-resource-isolator/docker/Dockerfile.libsync \
  -t <registry>/<project>/kai-resource-isolator-lib:<tag> \
  kai-resource-isolator

docker build -f kai-resource-isolator/docker/Dockerfile.webhook \
  -t <registry>/<project>/kai-resource-isolator-webhook:<tag> \
  kai-resource-isolator
```

Push the images:

```bash
docker push <registry>/<project>/kai-resource-isolator-lib:<tag>
docker push <registry>/<project>/kai-resource-isolator-webhook:<tag>
```

### Proxy and air-gapped builds

The webhook build uses `GOPROXY`. To change it, edit `ENV GOPROXY=...` in `docker/Dockerfile.webhook`, or pass `docker build --build-arg` after adding a matching `ARG` in the Dockerfile.

## Deploy with Helm

Chart path: `chart/kai-resource-isolator`.

1. **Image settings**: Point `librarySync.image` and `webhook.image` at your pushed images (and optionally set `global.imageRegistry` and `global.imagePullSecrets`) in `values.yaml` or at install time.

   Example overrides:

   ```bash
   helm upgrade --install kai-resource-isolator ./chart/kai-resource-isolator \
     --namespace kai-resource-isolator --create-namespace \
     --set librarySync.image.repository=<registry>/<project>/kai-resource-isolator-lib \
     --set librarySync.image.tag=<tag> \
     --set webhook.image.repository=<registry>/<project>/kai-resource-isolator-webhook \
     --set webhook.image.tag=<tag>
   ```

2. **Namespace**: Use `namespaceOverride` or Helm `--namespace` as needed.

3. **TLS**:
   - By default, the chart runs Jobs that patch webhook TLS material (`tls.patch.enabled: true` in `values.yaml`).
   - If you use **cert-manager**, set `tls.certManager.enabled: true`, set `tls.patch.enabled: false`, and configure `issuerRef` in `values.yaml`.

4. **Paths and resources**: Tune `paths.hostInstallBase`, `paths.containerVgpuMount`, and `webhook.gpuShareResources` for your environment and HAMi extended resource names.

After install, verify with `kubectl get daemonset`, `kubectl get mutatingwebhookconfiguration`, etc. Disable injection per Pod with annotation `kai-resource-isolator.io/inject: "false"`, or skip the webhook for a namespace with label `kai-resource-isolator.io/webhook=ignore`.

## Components (summary)

| Component | Role |
|-----------|------|
| DaemonSet (libsync) | Copies `libvgpu.so` to the configured path on each node (defaults follow `paths`) |
| Mutating webhook | Injects volumes, `ld.so.preload`, and related configuration for Pods that request the configured GPU-sharing resources |

For post-install hints, see the Helm-rendered **NOTES** printed after `helm install`.
