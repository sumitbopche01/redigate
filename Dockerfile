# Multi-stage build for the RediGate gateway binary.
# The runtime image is Alpine, runs as a non-root user, and trusts public CAs
# so it can call HTTPS model providers.

FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/redigate ./cmd/redigate

FROM alpine:3.20

RUN apk add --no-cache ca-certificates \
    && adduser -D -u 65532 nonroot

COPY --from=build /out/redigate /usr/local/bin/redigate

USER nonroot
EXPOSE 8080
ENV LISTEN_ADDR=:8080

HEALTHCHECK --interval=10s --timeout=2s --start-period=5s --retries=5 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["redigate"]
