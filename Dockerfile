# syntax=docker/dockerfile:1
ARG GO_VERSION=1.24.13

# base: the Go image, pinned by digest (the multi-arch index of golang:GO_VERSION-alpine, so
# amd64 and ARM builds both resolve). Bump the digest together with GO_VERSION; dependabot's
# docker ecosystem cannot see it through the ARG.
FROM golang:${GO_VERSION}-alpine@sha256:8bee1901f1e530bfb4a7850aa7a479d17ae3a18beb6e09064ed54cfd245b7191 AS base
ENV CGO_ENABLED=0 \
    GOFLAGS=-buildvcs=false \
    HOME=/tmp

# dev: toolchain image used by docker-compose (source is bind-mounted).
# Compose runs it as the host UID/GID, which the image cannot know, so the two cache
# directories (the mount points of the gomod/gocache volumes, which inherit their mode
# from here) are world-writable. Nothing else in the image is.
FROM base AS dev
RUN apk add --no-cache git make sqlite bash jq curl \
    && mkdir -p /go-cache /go/pkg/mod && chmod 777 /go-cache /go/pkg/mod
ENV GOCACHE=/go-cache
WORKDIR /app/src
CMD ["sh"]

# race: the dev image plus a C compiler, because the race detector needs cgo
# (the normal build stays static: CGO_ENABLED=0).
FROM dev AS race
USER root
RUN apk add --no-cache gcc musl-dev
ENV CGO_ENABLED=1

# build: reproducible static binary from the committed source.
FROM base AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG TARGETVARIANT
ARG VERSION=dev
ARG BUILD_DATE=
ENV GOCACHE=/tmp/go-cache \
    GOMODCACHE=/tmp/go-mod
RUN mkdir -p /out /app && chown nobody:nobody /out /app
USER nobody
COPY --chown=nobody:nobody src/ /app/src/
WORKDIR /app/src
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.buildDate=${BUILD_DATE}" -o /out/tack ./cmd/tack

# export: only the binary, for `docker build --target export --output bin .`
FROM scratch AS export
COPY --from=build /out/tack /tack
