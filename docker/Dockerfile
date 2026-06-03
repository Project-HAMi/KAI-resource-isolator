# Stage 1: Build libvgpu.so
FROM nvidia/cuda:12.9.1-cudnn-devel-ubuntu20.04 AS libvgpu-builder
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends cmake make gcc g++ && rm -rf /var/lib/apt/lists/*
WORKDIR /build/libvgpu
COPY libvgpu/ ./
RUN bash ./build.sh && test -f /build/libvgpu/build/libvgpu.so

# Stage 2: Build webhook
FROM golang:1.25-bookworm AS webhook-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /webhook ./cmd/webhook

# Stage 3: Final image
FROM ubuntu:24.04
RUN groupadd --system nonroot && useradd --system --gid nonroot --uid 65532 --no-create-home nonroot
COPY --from=libvgpu-builder /build/libvgpu/build/libvgpu.so /artifacts/libvgpu.so
COPY --from=webhook-builder /webhook /webhook
USER nonroot:nonroot
ENTRYPOINT ["/webhook"]
