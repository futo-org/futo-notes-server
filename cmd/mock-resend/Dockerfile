# mock-resend dev image. Build context is the repo root:
#   docker build -f cmd/mock-resend/Dockerfile .

ARG ALPINE_IMAGE=alpine:3.23@sha256:469c9ea410b9215eaee7976ba6c0639039b444be8e5d638f7abbada189971eb2

FROM golang:1.27.4-alpine3.23@sha256:f70850ef6ce341e8b6427f677049145d63c0b7ce002d4d7e3d464828b1b96dcd AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/mock-resend/ ./cmd/mock-resend/
COPY internal/mockresend/ ./internal/mockresend/
COPY internal/hostedenv/ ./internal/hostedenv/
COPY internal/httputil/ ./internal/httputil/
RUN CGO_ENABLED=0 go build -o /mock-resend ./cmd/mock-resend

FROM ${ALPINE_IMAGE}
RUN apk add --no-cache dumb-init \
    && addgroup -g 1000 mock-resend && adduser -u 1000 -G mock-resend -s /bin/sh -D mock-resend
USER mock-resend
COPY --from=builder /mock-resend /usr/local/bin/mock-resend
ENV MOCK_RESEND_PORT=8093
EXPOSE 8093
CMD ["dumb-init", "mock-resend"]
