# ── Build stage ────────────────────────────────────────────────────────────
FROM golang:1.22-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN GOOS=linux go build \
    -ldflags="-s -w" \
    -o /axon \
    ./cmd/axon

# ── Runtime stage ───────────────────────────────────────────────────────────
# Use ROCm base image so rocm-smi is available for AMD support.
# For NVIDIA-only, swap to a CUDA base image and remove the ROCm install.
FROM debian:bookworm-slim AS runtime

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    wget \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /axon /usr/local/bin/axon

EXPOSE 9101

# Requires: --pid=host, privileged (or cap_add SYS_PTRACE + access to /dev)
ENTRYPOINT ["/usr/local/bin/axon"]
