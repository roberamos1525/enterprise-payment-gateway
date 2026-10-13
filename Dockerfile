# syntax=docker/dockerfile:1
# NOTE: written carefully but NOT built locally (Docker is not available on
# the development machine). The binaries are pure Go (CGO_ENABLED=0).

ARG GO_VERSION=1.26.2

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=0.1.0
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ \
      ./cmd/gateway ./cmd/gatewayctl ./cmd/webhook-receiver

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget \
 && addgroup -S gateway && adduser -S -G gateway -h /home/gateway gateway \
 && mkdir -p /data && chown gateway:gateway /data
COPY --from=build /out/ /usr/local/bin/
USER gateway
ENV GATEWAY_DB_PATH=/data/gateway.db \
    GATEWAY_LISTEN_ADDR=0.0.0.0:8080
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/gateway"]
