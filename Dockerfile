# syntax=docker/dockerfile:1
ARG GO_VERSION=1.24.13

# dev: toolchain image used by docker-compose (source is bind-mounted).
FROM golang:${GO_VERSION}-alpine AS dev
RUN apk add --no-cache git make sqlite \
    && mkdir -p /go-cache /go/pkg/mod /app && chmod -R 777 /go-cache /go /app
ENV CGO_ENABLED=0 \
    GOFLAGS=-buildvcs=false \
    GOCACHE=/go-cache \
    HOME=/tmp
WORKDIR /app/src
CMD ["sh"]

# build: reproducible static binary from the committed source.
FROM dev AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG TARGETVARIANT
ARG VERSION=dev
ARG BUILD_DATE=
COPY src/ /app/src/
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.buildDate=${BUILD_DATE}" -o /out/tkan ./cmd/tkan

# export: only the binary, for `docker build --target export --output bin .`
FROM scratch AS export
COPY --from=build /out/tkan /tkan
