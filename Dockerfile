FROM oven/bun:1.3.14-slim AS builder
WORKDIR /app

COPY package.json bun.lock ./
RUN bun install --frozen-lockfile

COPY src/ src/
COPY tsconfig.json build.mjs ./

RUN bun run build

# Production stage — only the bundle + pg driver
FROM oven/bun:1.3.14-slim
ENV NODE_ENV=production
WORKDIR /app

LABEL org.opencontainers.image.title="FUTO Notes Server"
LABEL org.opencontainers.image.description="Self-hosted E2EE sync server for FUTO Notes"

RUN apt-get update && apt-get install -y --no-install-recommends gosu \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /app/dist/ dist/
COPY --from=builder /app/package.json ./
COPY --from=builder /app/bun.lock ./

RUN bun install --production --frozen-lockfile \
    && rm -rf /root/.bun/install/cache

ENV PORT=3000
ENV BLOB_DIR=/data/blobs
# AUTH_MODE selects the auth strategy ("password" vs "dev"); it is NOT a secret
# value, so the DS-0031 "secret in env" finding here is a false positive.
#trivy:ignore:DS-0031
ENV AUTH_MODE=password

# Start as root to chown the (bind-mounted) blob dir — a bind mount is created
# root-owned at runtime and masks this build-time chown — then drop to the
# unprivileged `bun` user (uid 1000) via gosu. Non-recursive: bun owns the dir,
# so blobs it writes underneath are bun-owned.
RUN mkdir -p $BLOB_DIR && chown -R bun:bun /data
ENTRYPOINT ["/bin/sh", "-c", "chown bun:bun /data $BLOB_DIR && exec gosu bun \"$@\"", "--"]

EXPOSE 3000

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD bun -e "fetch('http://localhost:3000/health').then(r=>{if(!r.ok)throw r;process.exit(0)}).catch(()=>process.exit(1))"

CMD ["bun", "dist/index.js"]
