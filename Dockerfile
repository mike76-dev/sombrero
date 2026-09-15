# syntax=docker/dockerfile:1

# The web UI is built first, since the server embeds it at compile time.
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /sombrero .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /sombrero /usr/local/bin/sombrero

# /data holds sombrero.yml and, in the Lite mode, store.json.
VOLUME /data
WORKDIR /data

# The server is meant to run on the host network, where this is only a label.
# The API port is left out: it stays on localhost unless sombrero.yml says otherwise.
EXPOSE 445

ENTRYPOINT ["/usr/local/bin/sombrero", "--dir=/data"]
