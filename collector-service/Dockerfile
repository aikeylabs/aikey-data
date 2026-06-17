# Multi-stage build — produces a minimal Alpine image.
# Requires Go 1.26+
#
# Build context must be the aikeylabs root (context: .. in docker-compose.yml)
# so that pkg/buildinfo/ is accessible.

# ---- build stage ----
FROM golang:1.26-alpine AS builder

ARG BUILD_VERSION=dev
ARG BUILD_REVISION=unknown
ARG BUILD_ID=unknown
ARG BUILD_TIME=unknown

WORKDIR /src

# Copy local module dependencies PRESERVING the repo-relative layout, so every
# `replace` resolves with no rewriting — including the cascade where
# aikey-config-tool (a collector production dep) itself replaces ../pkg/* and
# ../aikey-data/baseline. (Previously only buildinfo was copied+sed'd, so a
# clean build failed on the other local replaces — aikeytime/aikeycompat/
# usagehash/baseline/aikey-config-tool — and only cached builds succeeded.)
# Layout-preserving avoids editing every module's go.mod, which the cascade
# would otherwise require.
COPY pkg/buildinfo/ ./pkg/buildinfo/
COPY pkg/aikeytime/ ./pkg/aikeytime/
COPY pkg/aikeycompat/ ./pkg/aikeycompat/
COPY pkg/usagehash/ ./pkg/usagehash/
COPY aikey-config-tool/ ./aikey-config-tool/
COPY aikey-data/baseline/ ./aikey-data/baseline/
COPY aikey-data/collector-service/ ./aikey-data/collector-service/

WORKDIR /src/aikey-data/collector-service

RUN go mod download \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w \
      -X github.com/AiKeyLabs/pkg/buildinfo.Version=${BUILD_VERSION} \
      -X github.com/AiKeyLabs/pkg/buildinfo.Revision=${BUILD_REVISION} \
      -X github.com/AiKeyLabs/pkg/buildinfo.BuildID=${BUILD_ID} \
      -X github.com/AiKeyLabs/pkg/buildinfo.BuildTime=${BUILD_TIME}" \
    -o /app/collector-service ./cmd

# ---- runtime stage ----
FROM alpine:3.20

ARG BUILD_VERSION=dev
ARG BUILD_REVISION=unknown
ARG BUILD_ID=unknown
ARG BUILD_TIME=unknown
LABEL org.opencontainers.image.version="${BUILD_VERSION}"
LABEL org.opencontainers.image.revision="${BUILD_REVISION}"
LABEL com.aikeylabs.build-id="${BUILD_ID}"

WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata

RUN echo "${BUILD_VERSION}+${BUILD_ID} ${BUILD_TIME}" > /etc/aikey-version

COPY --from=builder /app/collector-service .
COPY aikey-data/collector-service/migrations/ ./migrations/

EXPOSE 27300
ENTRYPOINT ["/app/collector-service"]
