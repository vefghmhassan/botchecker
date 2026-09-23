# syntax=docker/dockerfile:1

FROM golang:1.23-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Pure-Go SQLite driver, so the binary is static and needs no build toolchain
# in the final image.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/botchecker ./cmd/server

FROM alpine:3.20
# tzdata lets DASHBOARD_TIMEZONE resolve; without it the service falls back to UTC.
RUN apk add --no-cache ca-certificates tzdata wget \
 && adduser -D -u 10001 botchecker \
 && mkdir -p /data && chown botchecker:botchecker /data

COPY --from=build /out/botchecker /usr/local/bin/botchecker

USER botchecker
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/botchecker"]
