FROM node:24-slim AS builder
WORKDIR /app

RUN corepack enable && corepack prepare pnpm@10 --activate

COPY package.json pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

COPY src/ src/
COPY tsconfig.json ./

RUN pnpm run build

# Production stage — only the bundle + pg driver
FROM node:24-slim
ENV NODE_ENV=production
WORKDIR /app

LABEL org.opencontainers.image.title="Stonefruit Server"
LABEL org.opencontainers.image.description="Self-hosted E2EE sync server for Stonefruit"

COPY --from=builder /app/dist/ dist/
COPY --from=builder /app/package.json ./
COPY --from=builder /app/pnpm-lock.yaml ./

RUN corepack enable && corepack prepare pnpm@10 --activate \
    && pnpm install --prod --frozen-lockfile \
    && rm -rf /root/.cache

ENV PORT=3000
ENV BLOB_DIR=/data/blobs
ENV AUTH_MODE=password

EXPOSE 3000

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD node -e "fetch('http://localhost:3000/health').then(r=>{if(!r.ok)throw r;process.exit(0)}).catch(()=>process.exit(1))"

CMD ["node", "dist/index.js"]
