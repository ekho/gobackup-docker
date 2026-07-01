# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gobackup-docker ./cmd/gobackup-docker

FROM alpine:3.20
# ca-certificates lets the Docker client reach a remote DOCKER_HOST over TLS.
RUN apk add --no-cache ca-certificates
COPY --from=build /out/gobackup-docker /usr/local/bin/gobackup-docker
# Runs as root for /var/run/docker.sock access. Harden in prod with a read-only
# docker-socket-proxy instead of mounting the socket directly.
ENTRYPOINT ["/usr/local/bin/gobackup-docker"]
