# syntax=docker/dockerfile:1.6

# --- builder ---------------------------------------------------------------
FROM golang:1.26-alpine AS builder

WORKDIR /src

# Pre-fetch modules in their own layer for better cache reuse.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .

# Build a fully static binary. modernc.org/sqlite is pure Go (no CGO),
# so CGO_ENABLED=0 works and the resulting binary runs in scratch/distroless.
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w -X github.com/vlauciani/odino/internal/cli.Version=${VERSION}" \
    -o /out/odino ./cmd/odino

# --- runtime --------------------------------------------------------------
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 1000 odino \
    && mkdir -p /var/cache/odino \
    && chown -R odino:odino /var/cache/odino

COPY --from=builder /out/odino /usr/local/bin/odino

USER odino
ENV ODINO_CACHE_DIR=/var/cache/odino
VOLUME ["/var/cache/odino"]

ENTRYPOINT ["/usr/local/bin/odino"]
CMD ["--help"]
