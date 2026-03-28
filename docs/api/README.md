# API Documentation

OpenAPI 3.0 specification for the Concord Voice Control Plane API.

## Files

- [openapi.yaml](./openapi.yaml) — OpenAPI spec (88 operations across 66 paths documented)

> **Note:** The spec documents 88 HTTP operations but there are 177 actual API routes in the codebase. Phase 2A additions (MFA, RBAC, media, ownership, email verification, client config, updates) are not yet in the spec. Full regeneration is tracked as a Phase 2B task.

## Viewing the Spec

**Swagger Editor** (browser):
```
https://editor.swagger.io
# Paste or import the openapi.yaml file
```

**Redoc CLI** (local):
```bash
npx @redocly/cli preview-docs docs/api/openapi.yaml
# Opens interactive docs at http://127.0.0.1:8080
```

**Validate**:
```bash
npx @redocly/cli lint docs/api/openapi.yaml
```

## Authentication Flow

1. **Register** or **Login** → receive JWT access token (15 min) + refresh token cookie (30 days)
2. Pass `Authorization: Bearer <token>` on all protected endpoints
3. When a 401 is returned, call `POST /auth/refresh` (sends cookie automatically) → new access token
4. **Logout** → invalidates the refresh token server-side

## Endpoint Groups

| Tag | Endpoints | Auth |
|-----|-----------|------|
| Auth | `/auth/{register,login,refresh,logout,ws-ticket}` | Public (rate-limited) |
| Users | `/users/me`, `/users/me/keys`, `/users/me/password`, `/users/me/preferences`, `/users/{id}/public-key` | Bearer |
| Sessions | `/sessions`, `/sessions/{id}` | Bearer |
| Servers | `/servers`, `/servers/unread-status`, `/servers/{id}` | Bearer |
| Channels | `/channels`, `/channels/{id}`, `/channels/{id}/{messages,read,keys}` | Bearer |
| Messages | `/messages`, `/messages/{id}` | Bearer |
| Members | `/servers/{id}/members`, `/servers/{id}/members/{userId}` | Bearer |
| Invites | `/servers/{id}/invites`, `/invites/join`, `/invites/{code}` | Bearer |
| E2EE | `/e2ee/pending-keys` | Bearer |
| WebSocket | `/ws` | Ticket or Bearer |

## Related Docs

- [Architecture](../architecture.md) — System diagrams, database ERD, message flows
- [Development Guide](../development.md) — Local setup, running tests
