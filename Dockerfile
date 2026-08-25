FROM golang:1.27-bookworm AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.1.0-dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.serverVersion=${VERSION}" \
    -o /out/futo-notes-server ./cmd/server

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gosu \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 1000 notes \
    && useradd --uid 1000 --gid notes --home-dir /nonexistent --shell /usr/sbin/nologin notes \
    && mkdir -p /data/blobs /data/db \
    && chown -R notes:notes /data

COPY --from=builder /out/futo-notes-server /usr/local/bin/futo-notes-server

ENV PORT=3000
ENV BLOB_DIR=/data/blobs
ENV DATABASE_URL=sqlite:/data/db/notes.db

EXPOSE 3000

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD curl --fail --silent --show-error http://127.0.0.1:3000/health >/dev/null || exit 1

# The previous TypeScript image wrote blobs as uid 1000. Keep that uid and
# repair only mount roots before dropping privileges; existing blob trees stay
# writable without an expensive recursive chown on every boot.
ENTRYPOINT ["/bin/sh", "-c", "mkdir -p /data/db \"$BLOB_DIR\" && chown notes:notes /data /data/db \"$BLOB_DIR\" && exec gosu notes \"$@\"", "--"]
CMD ["futo-notes-server"]
