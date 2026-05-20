# Build stage
FROM node:24-alpine AS builder

WORKDIR /app

# Install build dependencies (mediasoup needs Python, pip, and build tools)
RUN apk add --no-cache python3 py3-pip make g++ linux-headers

# stubs/ must precede npm ci — package.json overrides node-domexception → ./stubs/node-domexception
COPY package*.json ./
COPY stubs ./stubs
RUN npm ci

# Copy source code
COPY . .

# Build TypeScript
RUN npm run build

# Production stage
FROM node:24-alpine

WORKDIR /app

# Install runtime dependencies for mediasoup worker build
RUN apk add --no-cache python3 py3-pip make g++ linux-headers

# Copy package files and install production dependencies
# stubs/ must precede npm ci — package.json overrides node-domexception → ./stubs/node-domexception
# stubs/ must persist in the final image — npm creates a relative symlink resolved at runtime by fetch-blob
COPY package*.json ./
COPY stubs ./stubs
RUN npm ci --omit=dev

# Copy built application
COPY --from=builder /app/dist ./dist

# Run as non-root user
RUN addgroup -S appuser && adduser -S appuser -G appuser
RUN chown -R appuser:appuser /app
USER appuser

# Expose ports
EXPOSE 3000
EXPOSE 40000-49999/udp

# Run the application
CMD ["node", "dist/index.js"]
