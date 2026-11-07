# Multi-stage Dockerfile for G-Gate GPU Observability & QoS Governor Agent

FROM golang:1.25-bookworm AS builder

WORKDIR /src

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code including embedded eBPF objects in internal/bpf
COPY cmd/ cmd/
COPY internal/ internal/

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ggate-agent ./cmd/ggate-agent

# Runtime stage
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /
COPY --from=builder /out/ggate-agent /ggate-agent

USER 0:0

EXPOSE 9090

ENTRYPOINT ["/ggate-agent"]
