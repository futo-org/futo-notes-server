FROM node:24-slim AS builder
WORKDIR /app

RUN corepack enable && corepack prepare pnpm@10 --activate

COPY package.json pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

COPY src/ src/
COPY tsconfig.json build.mjs ./

RUN pnpm run build

# Production stage — only the bundle + pg driver
FROM node:24-slim
ENV NODE_ENV=production
WORKDIR /app

LABEL org.opencontainers.image.title="FUTO Notes Server"
LABEL org.opencontainers.image.description="Self-hosted E2EE sync server for FUTO Notes"

COPY --from=builder /app/dist/ dist/
COPY --from=builder /app/package.json ./
COPY --from=builder /app/pnpm-lock.yaml ./

RUN corepack enable && corepack prepare pnpm@10 --activate \
    && pnpm install --prod --frozen-lockfile \
    && rm -rf /root/.cache

ENV PORT=3000
ENV BLOB_DIR=/data/blobs
# AUTH_MODE selects the auth strategy ("password" vs "dev"); it is NOT a secret
# value, so the DS-0031 "secret in env" finding here is a false positive.
#trivy:ignore:DS-0031
ENV AUTH_MODE=password

# Drop privileges: run as the unprivileged `node` user (uid 1000) baked into
# the base image instead of root. The blob directory must be writable by it.
RUN mkdir -p /data/blobs && chown -R node:node /data
USER node

EXPOSE 3000

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD node -e "fetch('http://localhost:3000/health').then(r=>{if(!r.ok)throw r;process.exit(0)}).catch(()=>process.exit(1))"

CMD ["node", "dist/index.js"]
