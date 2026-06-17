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

# Copy local module dependencies (monorepo `replace` targets) first, for layer
# caching. ALL local replaces must be present or `go mod download` fails on a
# cache miss: buildinfo/aikeytime/aikeycompat are production deps, baseline is a
# test-only dep — but `go mod download` resolves the whole module graph (incl.
# test deps), so every replace target must exist. (Previously only buildinfo was
# copied+rewritten; clean builds failed on aikeytime/baseline, masked by cache.)
# These are leaf modules (no further local replaces), so no cascade.
COPY pkg/buildinfo/ /buildinfo/
COPY pkg/aikeytime/ /aikeytime/
COPY pkg/aikeycompat/ /aikeycompat/
COPY aikey-data/baseline/ /baseline/

# Copy service source
COPY aikey-data/query-service/ .

# Rewrite the dev-layout replace directives to the Docker-local copies.
RUN sed -i \
    -e 's|=> ../../pkg/buildinfo|=> /buildinfo|' \
    -e 's|=> ../../pkg/aikeytime|=> /aikeytime|' \
    -e 's|=> ../../pkg/aikeycompat|=> /aikeycompat|' \
    -e 's|=> ../baseline|=> /baseline|' \
    go.mod \
    && go mod download \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w \
      -X github.com/AiKeyLabs/pkg/buildinfo.Version=${BUILD_VERSION} \
      -X github.com/AiKeyLabs/pkg/buildinfo.Revision=${BUILD_REVISION} \
      -X github.com/AiKeyLabs/pkg/buildinfo.BuildID=${BUILD_ID} \
      -X github.com/AiKeyLabs/pkg/buildinfo.BuildTime=${BUILD_TIME}" \
    -o /app/query-service ./cmd

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

COPY --from=builder /app/query-service .

EXPOSE 27310
ENTRYPOINT ["/app/query-service"]
