# Build stage
FROM ghcr.io/markdrogersjr/node-buildtools:24-alpine-2026-05-28@sha256:a53963b772b7a3e09d1d82bf4fac9fd775350a45d94430527bdf76f8729a3fc2 AS builder

WORKDIR /app

# stubs/ must precede npm ci — package.json overrides node-domexception → ./stubs/node-domexception
COPY package*.json ./
COPY stubs ./stubs
RUN npm ci

# Copy source code
COPY . .

# Build TypeScript
RUN npm run build

# Drop dev deps after build, then verify the `file:` override symlink survived prune.
# Defense against future npm versions changing prune semantics in a way that
# would silently break the node-domexception override (per #1083 → #1097).
RUN npm prune --omit=dev \
 && test -L node_modules/node-domexception \
 && test -d node_modules/node-domexception/ \
 && npm ls node-domexception

# Production stage — NO compilation toolchain.
# mediasoup-worker (native ELF) was compiled in the builder stage above.
# node_modules carries the worker binary + pruned prod deps + the node-domexception override symlink.
# stubs/ must persist at runtime — fetch-blob resolves the symlink at require time.
# See [internal]rules/media-plane.md "Docker Build Context Invariant" for the discipline.
FROM node:24-alpine

WORKDIR /app

COPY --from=builder /app/node_modules ./node_modules
COPY --from=builder /app/dist ./dist
COPY --from=builder /app/stubs ./stubs
COPY --from=builder /app/package*.json ./

# Run as non-root user
RUN addgroup -S appuser && adduser -S appuser -G appuser
RUN chown -R appuser:appuser /app
USER appuser

# Expose ports
EXPOSE 3000
EXPOSE 40000-49999/udp

# Run the application
CMD ["node", "dist/index.js"]
