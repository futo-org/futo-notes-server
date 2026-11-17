# Storage service image. Build context is the repo root:
#   docker build -f cmd/storage/Dockerfile .

ARG ALPINE_IMAGE=alpine:3.23@sha256:469c9ea410b9215eaee7976ba6c0639039b444be8e5d638f7abbada189971eb2

FROM golang:1.27.4-alpine3.23@sha256:f70850ef6ce341e8b6427f677049145d63c0b7ce002d4d7e3d464828b1b96dcd AS builder
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/storage/ ./cmd/storage/
COPY internal/blobframe/ ./internal/blobframe/
COPY internal/hostedauth/ ./internal/hostedauth/
COPY internal/ids/ ./internal/ids/
COPY internal/storagesvc/ ./internal/storagesvc/
RUN CGO_ENABLED=0 go build -ldflags="-X main.version=${VERSION}" -o /storage ./cmd/storage

FROM ${ALPINE_IMAGE}
RUN apk add --no-cache dumb-init \
    && addgroup -g 1000 storage && adduser -u 1000 -G storage -s /bin/sh -D storage
USER storage
COPY --from=builder /storage /usr/local/bin/storage
ENV STORAGE_PORT=3015
EXPOSE 3015
CMD ["dumb-init", "storage"]
