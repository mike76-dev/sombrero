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

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
# timetzdata embeds the time zones, so that TZ can set the zone of the log timestamps.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -tags timetzdata -ldflags="-s -w" -o /sombrero .

# The base image already carries the CA certificates, and nothing is run in it.
FROM alpine:3.22
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
