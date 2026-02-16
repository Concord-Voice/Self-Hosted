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

#### Authentication (Phase 1A) ✅ IMPLEMENTED
```
POST /api/v1/auth/register
  - Registers new user with Argon2id password hashing
  - Validates username (profanity filtering, leetspeak detection)
  - Stores E2EE public key
  - Returns JWT access token + refresh token (HttpOnly cookie)

POST /api/v1/auth/login
  - Authenticates user
  - Returns JWT access token + refresh token
  - Returns E2EE keys (wrapped_private_key, key_derivation_salt)

POST /api/v1/auth/refresh
  - Refreshes access token using refresh token cookie

POST /api/v1/auth/logout
  - Revokes refresh token

GET /api/v1/auth/sessions (authenticated)
  - Lists user's active sessions

DELETE /api/v1/auth/sessions/:id (authenticated)
  - Revokes a specific session
```

#### Users (Phase 1A) ✅ IMPLEMENTED
```
GET /api/v1/users/me (authenticated)
  - Returns current user info
```

#### Servers (Phase 1B) 🚧 PLANNED
```
GET /api/v1/servers (authenticated)
POST /api/v1/servers (authenticated)
GET /api/v1/servers/:id (authenticated)
PATCH /api/v1/servers/:id (authenticated)
DELETE /api/v1/servers/:id (authenticated)
```

#### Channels (Phase 1B) 🚧 PLANNED
```
POST /api/v1/servers/:server_id/channels (authenticated)
GET /api/v1/servers/:server_id/channels (authenticated)
GET /api/v1/channels/:id (authenticated)
PATCH /api/v1/channels/:id (authenticated)
DELETE /api/v1/channels/:id (authenticated)
```

#### WebSocket (Phase 1B) 🚧 PLANNED
```
GET /api/v1/ws (authenticated)
  - Real-time messaging
  - Presence updates
  - Typing indicators
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

# Or use Make
make build

# Build Docker image
docker build -t concord-control-plane .
```

## Database Migrations

This service uses [golang-migrate](https://github.com/golang-migrate/migrate) for versioned database migrations with rollback support.

### Quick Start

```bash
# Apply all pending migrations
make migrate-up

# Rollback last migration
make migrate-down

# Check current version
make migrate-version

# Create new migration
make migrate-create NAME=add_user_status
```

### How Migrations Work

- Migrations run automatically when the server starts
- Each migration has an UP (apply) and DOWN (rollback) SQL file
- Migrations are versioned sequentially (000001, 000002, etc.)
- Migration state is tracked in the `schema_migrations` table

### Creating New Migrations

1. Use the Make command to generate migration files:
   ```bash
   make migrate-create NAME=add_user_profile_fields
   ```

2. Edit the generated files in `migrations/`:
   - `000004_add_user_profile_fields.up.sql` - Forward migration
   - `000004_add_user_profile_fields.down.sql` - Rollback migration

3. Apply the migration:
   ```bash
   make migrate-up
   ```

### Migration Best Practices

- Always create both UP and DOWN migrations
- Test rollbacks to ensure they properly reverse changes
- One logical change per migration
- Never modify committed migrations (create new ones instead)
- Migrations run in transactions automatically

For detailed migration documentation, see [migrations/README.md](migrations/README.md).

## Database Schema

### Users (Phase 1A) ✅
- id (UUID, PK)
- email (unique)
- username (unique, lowercase)
- password_hash (Argon2id)
- age_confirmed (boolean)
- e2ee_preference (boolean)
- created_at
- updated_at

### Public Keys (Phase 1A) ✅
- id (UUID, PK)
- user_id (FK → users)
- public_key (BYTEA, base64-encoded SPKI format)
- key_version (integer, default 1)
- created_at
- updated_at
- UNIQUE(user_id, key_version)

### Refresh Tokens (Phase 1A) ✅
- id (UUID, PK)
- user_id (FK → users)
- token_hash (SHA256)
- user_agent (text)
- ip_address (text)
- created_at
- expires_at

### Servers (Phase 1B) 🚧
- id (UUID, PK)
- name
- owner_id (FK → users)
- icon_url (text, nullable)
- created_at
- updated_at

### Channels (Phase 1B) 🚧
- id (UUID, PK)
- server_id (FK → servers, ON DELETE CASCADE)
- name
- type (text: 'text', 'voice', 'announcement')
- position (integer, default 0)
- created_at

### Server Members (Phase 1B) 🚧
- server_id (FK → servers, ON DELETE CASCADE)
- user_id (FK → users, ON DELETE CASCADE)
- role (text: 'owner', 'admin', 'member')
- joined_at
- PRIMARY KEY (server_id, user_id)

### Messages (Phase 1B) 🚧
- id (UUID, PK)
- channel_id (FK → channels, ON DELETE CASCADE)
- user_id (FK → users)
- content (text, encrypted if is_encrypted=true)
- is_encrypted (boolean, default false)
- edited_at (timestamp, nullable)
- created_at

## Current Status

### Phase 1A: Authentication & E2EE ✅ COMPLETE
- [x] User registration with Argon2id password hashing
- [x] Login with JWT access tokens (15 min) + refresh tokens (30 day)
- [x] Username validation with profanity filtering and leetspeak detection
- [x] E2EE public key storage
- [x] Session management (list/revoke sessions)
- [x] Rate limiting (Redis-based, per-IP and per-user)
- [x] Password strength validation

### Phase 1B: Channels & Text Chat 🚧 IN PROGRESS
- [ ] Server CRUD operations
- [ ] Channel CRUD operations
- [ ] WebSocket real-time messaging
- [ ] User presence tracking (online/offline/typing)
- [ ] Message history and pagination
- [ ] Basic permissions (owner, admin, member)

### Phase 1C: Voice Communication 📋 PLANNED
- [ ] WebSocket signaling for voice connections
- [ ] Integration with Media Plane

### Phase 2+: Advanced Features 📋 PLANNED
- [ ] Billing integration (Stripe)
- [ ] Advanced permissions system
- [ ] Audit logging
- [ ] File uploads (S3 + CDN)
- [ ] Message search
- [ ] LDAP/SAML integration for enterprise
- [ ] API rate limiting tiers and quotas
- [ ] Moderation tools (kick, ban, timeout)
