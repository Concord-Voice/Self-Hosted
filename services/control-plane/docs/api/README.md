# Control Plane API Documentation

Base URL: `http://localhost:8080/api/v1`

## Table of Contents

- [Authentication](#authentication)
- [Users](#users)
- [Servers](#servers)
- [Channels](#channels)
- [Members](#members)
- [Messages](#messages)
- [Sessions](#sessions)
- [WebSocket](#websocket)

## Authentication

### Register
```http
POST /auth/register
Content-Type: application/json

{
  "email": "user@example.com",
  "username": "username",
  "password": "secure_password"
}
```

**Response:**
```json
{
  "user": {
    "id": "uuid",
    "email": "user@example.com",
    "username": "username"
  },
  "access_token": "jwt_token",
  "refresh_token": "refresh_token"
}
```

### Login
```http
POST /auth/login
Content-Type: application/json

{
  "email": "user@example.com",
  "password": "password"
}
```

### Logout
```http
POST /auth/logout
Authorization: Bearer <access_token>
```

### Refresh Token
```http
POST /auth/refresh
Content-Type: application/json

{
  "refresh_token": "refresh_token"
}
```

## Users

### Get Current User
```http
GET /users/me
Authorization: Bearer <access_token>
```

## Servers

### List Servers
```http
GET /servers
Authorization: Bearer <access_token>
```

### Create Server
```http
POST /servers
Authorization: Bearer <access_token>
Content-Type: application/json

{
  "name": "My Server",
  "icon_url": "https://example.com/icon.png"
}
```

### Get Server
```http
GET /servers/:id
Authorization: Bearer <access_token>
```

### Update Server
```http
PATCH /servers/:id
Authorization: Bearer <access_token>
Content-Type: application/json

{
  "name": "Updated Name"
}
```

### Delete Server
```http
DELETE /servers/:id
Authorization: Bearer <access_token>
```

## Channels

### List Channels
```http
GET /servers/:id/channels
Authorization: Bearer <access_token>
```

### Create Channel
```http
POST /channels
Authorization: Bearer <access_token>
Content-Type: application/json

{
  "server_id": "uuid",
  "name": "general",
  "type": "text"
}
```

### Get Channel
```http
GET /channels/:id
Authorization: Bearer <access_token>
```

### Update Channel
```http
PATCH /channels/:id
Authorization: Bearer <access_token>
Content-Type: application/json

{
  "name": "updated-name"
}
```

### Delete Channel
```http
DELETE /channels/:id
Authorization: Bearer <access_token>
```

## Members

### List Members
```http
GET /servers/:id/members
Authorization: Bearer <access_token>
```

### Add Member
```http
POST /servers/:id/members
Authorization: Bearer <access_token>
Content-Type: application/json

{
  "user_id": "uuid",
  "role": "member"
}
```

### Update Member Role
```http
PATCH /servers/:id/members/:user_id
Authorization: Bearer <access_token>
Content-Type: application/json

{
  "role": "admin"
}
```

### Remove Member
```http
DELETE /servers/:id/members/:user_id
Authorization: Bearer <access_token>
```

## Messages

### Get Messages
```http
GET /channels/:id/messages?limit=50&before=<message_id>
Authorization: Bearer <access_token>
```

**Query Parameters:**
- `limit`: Number of messages (max 100, default 50)
- `before`: Message ID for pagination

### Send Message
```http
POST /messages
Authorization: Bearer <access_token>
Content-Type: application/json

{
  "channel_id": "uuid",
  "content": "Hello, world!",
  "is_encrypted": false
}
```

### Update Message
```http
PATCH /messages/:id
Authorization: Bearer <access_token>
Content-Type: application/json

{
  "content": "Updated message"
}
```

### Delete Message
```http
DELETE /messages/:id
Authorization: Bearer <access_token>
```

## Sessions

### List Active Sessions
```http
GET /sessions
Authorization: Bearer <access_token>
```

### Revoke Session
```http
DELETE /sessions/:id
Authorization: Bearer <access_token>
```

### Revoke All Sessions
```http
DELETE /sessions
Authorization: Bearer <access_token>
```

## WebSocket

### Connect
```
ws://localhost:8080/api/v1/ws?token=<jwt_token>
```

### Message Types

**Subscribe to Channel:**
```json
{
  "type": "subscribe",
  "channel_id": "uuid"
}
```

**Unsubscribe from Channel:**
```json
{
  "type": "unsubscribe",
  "channel_id": "uuid"
}
```

**Send Message:**
```json
{
  "type": "message",
  "channel_id": "uuid",
  "content": "Hello!",
  "is_encrypted": false
}
```

**Typing Indicator:**
```json
{
  "type": "typing",
  "channel_id": "uuid"
}
```

## Rate Limits

- **Auth endpoints:** 5-30 requests per 15 minutes
- **Read operations:** 30 requests per minute
- **Write operations:** 10 requests per minute
- **Destructive operations:** 5 requests per minute

## Error Responses

```json
{
  "error": "Error message description"
}
```

**Common Status Codes:**
- `200 OK` - Success
- `201 Created` - Resource created
- `400 Bad Request` - Invalid input
- `401 Unauthorized` - Missing or invalid token
- `403 Forbidden` - Insufficient permissions
- `404 Not Found` - Resource not found
- `429 Too Many Requests` - Rate limit exceeded
- `500 Internal Server Error` - Server error

## Postman Collection

Import the Postman collection from: `docs/api/Concord.postman_collection.json`

## Further Reading

- [Architecture Overview](../../../../docs/architecture.md)
- [OpenAPI Spec](../../../../docs/api/openapi.yaml)
- [Development Guide](../../../../docs/development.md)
