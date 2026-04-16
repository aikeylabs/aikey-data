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

# Copy buildinfo dependency first (for layer caching)
COPY pkg/buildinfo/ /buildinfo/

# Copy service source
COPY aikey-data/query-service/ .

# Fix replace directive: point to Docker-local path instead of dev layout
RUN sed -i 's|=> ../../pkg/buildinfo|=> /buildinfo|' go.mod \
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
