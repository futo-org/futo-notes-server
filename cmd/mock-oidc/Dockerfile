# mock-oidc dev image. Build context is the repo root:
#   docker build -f cmd/mock-oidc/Dockerfile .

ARG ALPINE_IMAGE=alpine:3.23@sha256:469c9ea410b9215eaee7976ba6c0639039b444be8e5d638f7abbada189971eb2

FROM golang:1.27.4-alpine3.23@sha256:f70850ef6ce341e8b6427f677049145d63c0b7ce002d4d7e3d464828b1b96dcd AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/mock-oidc/ ./cmd/mock-oidc/
# All of internal/, like the API image: a hand-kept package list breaks the
# build whenever an import moves.
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -o /mock-oidc ./cmd/mock-oidc

FROM ${ALPINE_IMAGE}
RUN apk add --no-cache dumb-init \
    && addgroup -g 1000 mock-oidc && adduser -u 1000 -G mock-oidc -s /bin/sh -D mock-oidc
USER mock-oidc
COPY --from=builder /mock-oidc /usr/local/bin/mock-oidc
ENV MOCK_OIDC_PORT=8092
EXPOSE 8092
CMD ["dumb-init", "mock-oidc"]
