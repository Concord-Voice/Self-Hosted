# Build stage
FROM node:20-alpine3.21 AS builder

WORKDIR /app

# Install build dependencies (mediasoup needs Python and build tools)
RUN apk add --no-cache python3 make g++ linux-headers

# Copy package files
COPY package*.json ./
RUN npm ci

# Copy source code
COPY . .

# Build TypeScript
RUN npm run build

# Production stage
FROM node:20-alpine3.21

WORKDIR /app

# Install runtime dependencies for mediasoup
RUN apk add --no-cache python3

# Copy package files and install production dependencies
COPY package*.json ./
RUN npm ci --only=production

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
