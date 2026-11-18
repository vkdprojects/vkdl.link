# syntax=docker/dockerfile:1

# --- build ---
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/vkdl ./cmd/vkdl

# --- runtime ---
FROM alpine:3.22
# wget (busybox) fica disponível para o healthcheck do Coolify, que roda dentro do container.
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S -g 10001 vkdl \
 && adduser -S -D -H -u 10001 -G vkdl -s /sbin/nologin vkdl \
 && mkdir -p /data \
 && chown 10001:10001 /data \
 && chmod 0750 /data
# Criado e com dono ajustado ANTES do VOLUME: um volume nomeado novo herda
# esse dono, então o usuário sem privilégios consegue gravar nele.
COPY --from=build /out/vkdl /usr/local/bin/vkdl
ENV VKDL_DATA_DIR=/data \
    VKDL_LISTEN=:8080
VOLUME /data
EXPOSE 8080
USER 10001:10001
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/vkdl"]
