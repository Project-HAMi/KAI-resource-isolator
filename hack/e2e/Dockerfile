# Copyright The HAMi Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Test image for hack/e2e/kind-smoke.sh. Same layout as the final stage of
# docker/Dockerfile, but libvgpu.so is copied from the published multi-arch
# image instead of being built, so the smoke test needs no CUDA toolchain.
# Build from the repository root:
#   docker build -f hack/e2e/Dockerfile -t kai-resource-isolator:e2e .

ARG LIBVGPU_IMAGE=docker.io/projecthami/kai-resource-isolator:v1.1.0

FROM ${LIBVGPU_IMAGE} AS libvgpu

FROM golang:1.25-bookworm AS go-builder
RUN apt-get update && apt-get install -y --no-install-recommends gcc libc6-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY pkg/ ./pkg/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /webhook ./cmd/webhook
# go-nvml requires CGO for generated bindings; libnvidia-ml is dlopened at runtime.
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /monitor ./cmd/monitor

FROM ubuntu:24.04
RUN groupadd --system nonroot && useradd --system --gid nonroot --uid 65532 --no-create-home nonroot
COPY --from=libvgpu /artifacts/libvgpu.so /artifacts/libvgpu.so
COPY --from=go-builder /webhook /webhook
COPY --from=go-builder /monitor /monitor
USER nonroot:nonroot
ENTRYPOINT ["/webhook"]
