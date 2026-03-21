# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application and tools
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o main ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o migrate-media ./cmd/migrate-media

# Final stage
FROM alpine:3.21

RUN apk --no-cache add ca-certificates

# Run as non-root user
RUN addgroup -S appuser && adduser -S appuser -G appuser
WORKDIR /home/appuser

# Copy binaries from builder
COPY --from=builder /app/main .
COPY --from=builder /app/migrate-media .

# Copy migrations
COPY --from=builder /app/migrations ./migrations

USER appuser

# Expose port
EXPOSE 8080

# Run the binary
CMD ["./main"]
