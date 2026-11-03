# syntax=docker/dockerfile:1

# The build stages run on the build machine's own platform and cross-compile,
# so an image for another platform needs no emulation.

# The web UI is built first, since the server embeds it at compile time.
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27-trixie AS build
ARG TARGETOS TARGETARCH BUILDARCH
# The Rust engine makes every build a cgo build, so a foreign target needs its C cross-compiler.
RUN case "$TARGETARCH" in "$BUILDARCH") ;; \
    arm64) apt-get update && apt-get install -y --no-install-recommends gcc-aarch64-linux-gnu libc6-dev-arm64-cross ;; \
    amd64) apt-get update && apt-get install -y --no-install-recommends gcc-x86-64-linux-gnu libc6-dev-amd64-cross ;; esac
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
# timetzdata embeds the time zones, so that TZ can set the zone of the log timestamps.
RUN case "$TARGETARCH" in "$BUILDARCH") ;; arm64) export CC=aarch64-linux-gnu-gcc ;; amd64) export CC=x86_64-linux-gnu-gcc ;; esac \
    && CGO_ENABLED=1 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -tags timetzdata -ldflags="-s -w" -o /sombrero .

# The storage engine's prebuilt libraries need glibc 2.38+, so the image is Debian 13, not Alpine.
# Nothing is run in it, so no emulation is ever needed; the CA certificates are copied in.
FROM debian:trixie-slim
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /sombrero /usr/local/bin/sombrero

# Links the published package to the repository on GitHub.
LABEL org.opencontainers.image.source=https://github.com/mike76-dev/sombrero

# /data holds sombrero.yml and, in the Lite mode, store.json.
VOLUME /data
WORKDIR /data

# The server is meant to run on the host network, where this is only a label.
# The API port is left out: it stays on localhost unless sombrero.yml says otherwise.
EXPOSE 445

ENTRYPOINT ["/usr/local/bin/sombrero", "--dir=/data"]
