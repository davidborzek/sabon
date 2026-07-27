# From-source build (used by `docker compose build` and local dev).
# The goreleaser build lives in ./Dockerfile.goreleaser.

# renovate: datasource=docker depName=restic/restic
FROM restic/restic:0.18.0 AS restic

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /sabon ./cmd/sabon

FROM gcr.io/distroless/static:nonroot
# sabon shells out to restic; bundle the static restic binary (single image,
# single restic version — movers reuse this exact image).
COPY --from=restic /usr/bin/restic /usr/bin/restic
COPY --from=build /sabon /sabon
USER nonroot:nonroot
ENTRYPOINT ["/sabon"]
