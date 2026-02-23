# Control Plane Tests

Test suite for the Concord Control Plane service (Go).

## Structure

```
internal/
├── testhelpers/                    # Shared test infrastructure
│   ├── testdb.go                   # SetupTestDB, TruncateAllTables, RunMigrations
│   ├── testredis.go                # SetupTestRedis (DB index 1, FLUSHDB cleanup)
│   ├── testserver.go               # TestServer with CreateTestUser/Server/Channel helpers
│   └── fixtures.go                 # TestUser struct, E2EETestKeys, ValidCiphertext
├── auth/
│   ├── password_test.go            # Argon2id hashing, password strength validation
│   ├── username_test.go            # Username validation, profanity filter, normalization
│   ├── tokens_test.go              # JWT generation/validation, refresh tokens
│   └── handlers_integration_test.go # Register, login, refresh, logout HTTP tests
├── channels/
│   └── handlers_test.go            # CRUD, unread tracking, E2EE key distribution
├── messages/
│   └── handlers_test.go            # Send/edit/delete, pagination, E2EE enforcement
├── servers/
│   └── handlers_test.go            # CRUD, role-based access, membership
├── members/
│   └── handlers_test.go            # Add/remove/update roles, kick, leave
├── invites/
│   └── handlers_test.go            # Create/revoke/join, expiry, max uses
├── users/
│   └── handlers_test.go            # Profile, preferences, password change, public keys
├── sessions/
│   ├── ip_test.go                  # IP masking (IPv4/IPv6)
│   └── handlers_test.go            # Session listing, revocation
├── middleware/
│   └── middleware_test.go          # Auth required, rate limiting
pkg/
└── config/
    └── config_test.go              # Config defaults, env overrides, validation
```

## Running Tests

```bash
cd services/control-plane

# Run all tests
go test ./...

# Run with race detection
go test -race ./...

# Run with verbose output
go test -v ./...

# Run specific package
go test ./internal/auth/...
go test ./internal/channels/...

# Run unit tests only (no DB required)
go test ./internal/auth/ -run "TestHashPassword|TestValidatePassword|TestValidateUsername|TestGenerate|TestValidateAccessToken"
go test ./internal/sessions/ -run "TestMaskIP"
go test ./pkg/config/...

# Run integration tests (requires PostgreSQL + Redis)
go test ./internal/auth/ -run "Integration"
go test ./internal/channels/... ./internal/messages/... ./internal/servers/...

# Coverage report
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
go tool cover -html=coverage.out  # Open in browser
```

## Test Infrastructure

### Test Helpers (`internal/testhelpers/`)

**SetupTestDB(t)** — Connects to PostgreSQL via `DATABASE_URL` env var (default: localhost), runs all migrations, returns DB handle + cleanup function that truncates all tables.

**SetupTestRedis(t)** — Connects to Redis via `REDIS_URL` env var, uses DB index 1 for isolation, cleanup calls FLUSHDB.

**SetupTestServer(t)** — Creates a full test server with Gin router, Hub, DB, Redis, and JWT secret. Provides convenience methods:
- `CreateTestUser(t, username)` — Inserts user with pre-computed Argon2id hash (avoids ~100ms per user), generates JWT
- `CreateTestServer(t, ownerID, name)` — Inserts server + owner membership
- `CreateTestChannel(t, serverID, name, isEncrypted)` — Inserts channel
- `AuthHeaders(userID)` — Returns `Authorization: Bearer <jwt>` header map
- `DoRequest(method, path, body, headers)` — HTTP request via `httptest.ResponseRecorder`

**Fixtures** — `E2EETestKeys()` for structurally-valid test keys, `ValidCiphertext()` for base64-encoded data passing minimum AES-GCM size validation.

### Prerequisites

Integration tests require running PostgreSQL and Redis:

```bash
docker-compose up -d postgres redis
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `DATABASE_URL` | `postgres://concord:concord_dev_password@localhost:5432/concord?sslmode=disable` | Test database |
| `REDIS_URL` | `redis://localhost:6379` | Test Redis (uses DB 1) |

## CI/CD

Tests run automatically on pull requests and pushes to main/develop. See `.github/workflows/ci.yml`.

Coverage threshold: **80%+** enforced in CI.
