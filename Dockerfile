# From-source build (used by `docker compose build` and local dev).
# The goreleaser build lives in ./Dockerfile.goreleaser.

# renovate: datasource=docker depName=restic/restic
FROM restic/restic:0.19.1 AS restic

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
# Cross-compile on the native build platform (no QEMU): CGO is off, so GOOS/
# GOARCH alone retarget. Compiling in the target-arch stage would emulate the
# Go compiler under buildx and is slow — the goreleaser image copies prebuilt
# binaries for the same reason.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-s -w -X main.version=${VERSION}" -o /sabon ./cmd/sabon

FROM gcr.io/distroless/static:nonroot
# sabon shells out to restic; bundle the static restic binary (single image,
# single restic version — movers reuse this exact image).
COPY --from=restic /usr/bin/restic /usr/bin/restic
COPY --from=build /sabon /sabon
USER nonroot:nonroot
# Distroless has no shell, curl or wget: sabon probes its own /readyz.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["/sabon", "healthcheck"]
ENTRYPOINT ["/sabon"]
