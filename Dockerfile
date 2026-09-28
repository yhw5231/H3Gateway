# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# build stage
#
# The gateway is deliberately dependency-free (standard library only), so the
# build needs no module download and works on an offline host.
# ---------------------------------------------------------------------------
FROM golang:1.23-alpine AS build

WORKDIR /src

# Only the module file is needed: go.mod lists no requirements.
COPY go.mod ./
COPY main.go ./
COPY internal ./internal
COPY web ./web

ARG VERSION=docker
ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/h3gateway .

# ---------------------------------------------------------------------------
# runtime stage
# ---------------------------------------------------------------------------
FROM alpine:latest

# ca-certificates: the gateway talks to the upstream over TLS.
# tzdata: correct timestamps in the console.
# wget (busybox): used by the health check.
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 h3 \
    && adduser -S -u 10001 -G h3 -h /data h3

WORKDIR /app
COPY --from=build /out/h3gateway /usr/local/bin/h3gateway

# Everything mutable lives under /data so a single volume covers it.
ENV GATEWAY_HOST=0.0.0.0 \
    GATEWAY_PORT=8787 \
    GATEWAY_DATA_DIR=/data \
    GATEWAY_LOG_LEVEL=info \
    TZ=Asia/Shanghai

RUN mkdir -p /data && chown -R h3:h3 /data
VOLUME ["/data"]

USER h3
EXPOSE 8787

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8787/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/h3gateway"]
CMD ["serve"]
