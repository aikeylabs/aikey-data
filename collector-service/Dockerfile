# Multi-stage build — produces a minimal Alpine image.
# Requires Go 1.26+

# ---- build stage ----
FROM golang:1.26-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /app/collector-service ./cmd

# ---- runtime stage ----
FROM alpine:3.20
WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /app/collector-service .
COPY migrations/ ./migrations/

EXPOSE 27300
ENTRYPOINT ["/app/collector-service"]
