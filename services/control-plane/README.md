# Control Plane Service

The control plane handles authentication, authorization, server/channel management, billing, and user presence for Concord.

## Tech Stack

- **Go** 1.21+
- **Gin** - HTTP web framework
- **PostgreSQL** - Primary database
- **Redis** - Caching and ephemeral state
- **JWT** - Authentication tokens
- **WebSocket** - Real-time signaling

## Architecture

The control plane is responsible for:
- User authentication and authorization
- Server and channel CRUD operations
- Membership and permissions management
- WebSocket signaling for voice connections
- Session management and presence
- Billing and subscription management (future)
- License validation coordination (future)

## Project Structure

```
control-plane/
├── cmd/
│   └── server/           # Application entry point
│       └── main.go
├── internal/
│   ├── api/              # HTTP handlers and routes
│   ├── auth/             # Authentication logic
│   ├── channels/         # Channel management
│   ├── database/         # Database connections
│   ├── middleware/       # HTTP middleware
│   ├── models/           # Data models
│   └── websocket/        # WebSocket handling
├── pkg/
│   ├── config/           # Configuration management
│   └── logger/           # Logging utilities
├── migrations/           # Database migrations
├── Dockerfile
├── go.mod
└── go.sum
```

## Development

### Prerequisites

- Go 1.21+
- PostgreSQL 15+
- Redis 7+

### Setup

1. **Install dependencies**
   ```bash
   go mod download
   ```

2. **Set up environment variables**
   ```bash
   cp .env.example .env
   # Edit .env with your configuration
   ```

3. **Start infrastructure** (using Docker Compose from project root)
   ```bash
   cd ../..
   docker-compose up -d postgres redis
   ```

4. **Run the service**
   ```bash
   go run cmd/server/main.go
   ```

The service will start on port 8080 by default.

### Environment Variables

```bash
ENVIRONMENT=development
PORT=8080
DATABASE_URL=postgres://concord:password@localhost:5432/concord?sslmode=disable
REDIS_URL=redis://localhost:6379
JWT_SECRET=your-secret-key
NATS_URL=nats://localhost:4222
MEDIA_PLANE_URL=http://localhost:3000
LICENSING_AUTHORITY_URL=http://localhost:8082
```

### Available Endpoints

#### Health Check
```
GET /health
```

#### Authentication
```
POST /api/v1/auth/register
POST /api/v1/auth/login
POST /api/v1/auth/logout
```

#### Users
```
GET /api/v1/users/me (authenticated)
```

#### Servers
```
GET /api/v1/servers (authenticated)
POST /api/v1/servers (authenticated)
GET /api/v1/servers/:id (authenticated)
```

#### Channels
```
POST /api/v1/channels (authenticated)
GET /api/v1/channels/:id (authenticated)
```

#### WebSocket
```
GET /api/v1/ws (authenticated)
```

### Testing

```bash
# Run tests
go test ./...

# Run tests with coverage
go test -cover ./...

# Run tests with race detector
go test -race ./...
```

### Building

```bash
# Build binary
go build -o bin/control-plane cmd/server/main.go

# Build Docker image
docker build -t concord-control-plane .
```

## Database Schema

### Users
- id (UUID, PK)
- email (unique)
- username (unique)
- password_hash
- created_at
- updated_at

### Servers
- id (UUID, PK)
- name
- owner_id (FK → users)
- created_at
- updated_at

### Channels
- id (UUID, PK)
- server_id (FK → servers)
- name
- type (voice, text)
- created_at
- updated_at

### Server Members
- server_id (FK → servers)
- user_id (FK → users)
- role (owner, admin, member)
- joined_at

## Future Enhancements

- [ ] Implement full authentication handlers
- [ ] Add server and channel CRUD operations
- [ ] WebSocket signaling for voice connections
- [ ] Rate limiting
- [ ] User presence tracking
- [ ] Billing integration (Stripe)
- [ ] Advanced permissions system
- [ ] Audit logging
- [ ] LDAP/SAML integration for enterprise
- [ ] API rate limiting and quotas
