# 从仓库根目录构建：docker build -f extensions/accessgateway/deploy/gateway/Dockerfile .
ARG GO_VERSION=1.26.4
FROM golang:${GO_VERSION}-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH
ARG GATEWAY_VERSION=dev
ARG SOURCE_COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download github.com/google/uuid github.com/gorilla/websocket github.com/pelletier/go-toml/v2
COPY extensions/accessgateway/protocol ./extensions/accessgateway/protocol
COPY extensions/accessgateway/transport ./extensions/accessgateway/transport
COPY extensions/accessgateway/gateway ./extensions/accessgateway/gateway
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags="-s -w -X main.gatewayVersion=${GATEWAY_VERSION} -X main.sourceCommit=${SOURCE_COMMIT}" \
    -o /out/owl-gateway ./extensions/accessgateway/gateway/cmd/owl-gateway

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && addgroup -g 10001 gateway && adduser -D -H -u 10001 -G gateway gateway \
    && mkdir -p /var/lib/owl-gateway /etc/owl-gateway/tls \
    && chown 10001:10001 /var/lib/owl-gateway && chmod 0700 /var/lib/owl-gateway
COPY --from=builder /out/owl-gateway /usr/local/bin/owl-gateway
USER 10001:10001
ENTRYPOINT ["owl-gateway"]
CMD ["-config", "/etc/owl-gateway/gateway.toml"]
