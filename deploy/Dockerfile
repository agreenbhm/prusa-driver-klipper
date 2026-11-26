# syntax=docker/dockerfile:1
#
# The published Prusa Connect image with its server binary swapped for one
# that has the Klipper driver compiled in. Everything else (the bundled
# extensions, the entry point, the health check, the user) is the stock image.
#
# Build from the root of this repository:
#
#   docker buildx build -f deploy/Dockerfile \
#     --build-arg PRUSA_CONNECT_VERSION=0.1.0-beta.2 \
#     --platform linux/amd64,linux/arm64 -t <registry>/prusa-connect-klipper:0.1.0-beta.2 --push .
#
# PRUSA_CONNECT_VERSION must be the version you run today: the base image and
# the core source are both taken at that tag, so the database schema matches.

ARG PRUSA_CONNECT_VERSION=0.1.0-beta.2
ARG GO_VERSION=1.26.11

# The core source at the release tag.
FROM --platform=$BUILDPLATFORM alpine/git:latest AS source
ARG PRUSA_CONNECT_VERSION
RUN git clone --depth 1 --branch "v${PRUSA_CONNECT_VERSION}" \
      https://github.com/prusa3d/prusa-connect.git /src/prusa-connect

# The web app, built the way the official image builds it. It does not depend
# on the target platform, so it is built once on the builder's own.
FROM --platform=$BUILDPLATFORM node:24-alpine AS app
ENV PNPM_HOME=/pnpm PATH=/pnpm:$PATH HUSKY=0 CI=true
RUN corepack enable
COPY --from=source /src/prusa-connect /app
WORKDIR /app
RUN cd packages/typescript && pnpm install --frozen-lockfile && pnpm run build
RUN cd packages/svelte && pnpm install --frozen-lockfile --ignore-scripts && pnpm run build
RUN cd packages/fractal/client && pnpm install --frozen-lockfile && pnpm run build
RUN cd frontend && pnpm install --frozen-lockfile && pnpm run build && test -f dist/index.html

# The server with the driver compiled in, cross-compiled for the target on the
# builder's own platform. compose-build runs helper programs with `go run`, and
# those must run here, so a wrapper drops the target from `go run` and keeps it
# for everything else.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-trixie AS build
ARG PRUSA_CONNECT_VERSION
ARG TARGETOS TARGETARCH TARGETVARIANT
RUN mkdir -p /opt/gowrap && printf '%s\n' '#!/bin/sh' \
      'if [ "$1" = run ]; then exec env -u GOOS -u GOARCH -u GOARM /usr/local/go/bin/go "$@"; fi' \
      'exec /usr/local/go/bin/go "$@"' > /opt/gowrap/go && chmod +x /opt/gowrap/go
COPY --from=source /src/prusa-connect /src/prusa-connect
COPY --from=app /app/frontend/dist /src/prusa-connect/internal/transport/http/httpapi/appdist
WORKDIR /src/prusa-connect
RUN go run ./cmd/compress-app ./internal/transport/http/httpapi/appdist && \
    go build -o /usr/local/bin/compose-build ./cmd/compose-build
COPY . /src/prusa-driver-klipper
# The build tags the official image carries: Postgres, diagnostics, metrics.
RUN printf '\nextra_tags: diagnostics observability postgres\n' >> /src/prusa-driver-klipper/build.yaml
RUN PATH="/opt/gowrap:$PATH" GOOS="$TARGETOS" GOARCH="$TARGETARCH" GOARM="${TARGETVARIANT#v}" CGO_ENABLED=0 \
    GOFLAGS="-ldflags=-X=github.com/prusa3d/prusa-connect/internal/kernel/version.stamped=${PRUSA_CONNECT_VERSION}" \
    compose-build --config ../prusa-driver-klipper/build.yaml && \
    test -f bin/server-klipper

FROM docker.io/prusaresearch/prusa-connect:${PRUSA_CONNECT_VERSION}
COPY --from=build /src/prusa-connect/bin/server-klipper /prusa_connect
