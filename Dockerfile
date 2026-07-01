# syntax=docker/dockerfile:1

# Cross-compile on the native build platform (no QEMU needed): the build stage
# always runs on $BUILDPLATFORM and targets $TARGETARCH via Go's GOARCH.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/gobackup-docker ./cmd/gobackup-docker

# Final image: scratch + the static binary + CA certs (for a remote TLS DOCKER_HOST).
# Copy-only, so nothing executes under emulation and arm64 builds need no QEMU.
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/gobackup-docker /usr/local/bin/gobackup-docker
# Runs as root (uid 0) for /var/run/docker.sock access. Harden in prod with a
# read-only docker-socket-proxy instead of mounting the socket directly.
ENTRYPOINT ["/usr/local/bin/gobackup-docker"]
