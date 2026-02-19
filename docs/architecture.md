# Concord Architecture

## Overview

Concord is a distributed real-time communications platform with clear separation of concerns across three major planes:

1. **Client Layer** - User-facing applications
2. **Control Plane** - Business logic, auth, state management
3. **Media Plane** - WebRTC media routing
4. **Licensing Authority** - License management for self-hosted

## High-Level Architecture

```
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│   Desktop    │     │   Web App    │     │   Mobile     │
│    Client    │     │              │     │   (Future)   │
└──────┬───────┘     └──────┬───────┘     └──────┬───────┘
       │                    │                     │
       └────────────────────┼─────────────────────┘
                            │
                ┌───────────┴───────────┐
                │                       │
        ┌───────▼────────┐      ┌──────▼──────┐
        │ Control Plane  │      │ Media Plane │
        │   (Go HTTP)    │      │  (Node.js)  │
        └───────┬────────┘      └──────┬──────┘
                │                      │
        ┌───────┴────────┐            │
        │                │            │
   ┌────▼─────┐    ┌────▼─────┐      │
   │PostgreSQL│    │  Redis   │◄─────┘
   └──────────┘    └──────────┘

Self-Hosted Instances:
┌────────────────────────────────┐
│  Self-Hosted Concord Server    │
│  ┌──────────┬──────────┐       │
│  │ Control  │  Media   │       │
│  │  Plane   │  Plane   │       │
│  └─────┬────┴────┬─────┘       │
│        │         │             │
│   ┌────▼────┬────▼────┐        │
│   │  Postgres  │ Redis │       │
│   └────────────┴───────┘       │
└────────┬───────────────────────┘
         │ Check-in (periodic)
         ▼
┌──────────────────────┐
│ Licensing Authority  │
│   (Validates licenses)│
└──────────────────────┘
```

## Component Details

### Client Layer

#### Desktop Client (Electron + React + TypeScript)
- **Technology**: Electron, React, TypeScript, WebRTC
- **Responsibilities**:
  - User interface
  - WebRTC media handling (Opus codec)
  - Push-to-talk
  - Noise suppression
  - Authentication UI
  - Channel navigation
- **Communication**:
  - HTTP/WebSocket → Control Plane (signaling, auth, channels)
  - WebRTC → Media Plane (audio streams)

#### Web Client (React + TypeScript)
- Same as desktop but without Electron wrapper
- Progressive Web App (PWA) capabilities
- Limited platform integration

### Control Plane (Go)

**Port**: 8080

**Tech Stack**:
- Go 1.24+
- Gin web framework
- PostgreSQL 16 (persistent data)
- Redis 7 (ephemeral state, sessions, rate limiting)
- gorilla/websocket (real-time messaging)

**Responsibilities**:
- User authentication (JWT)
- Server/channel CRUD operations
- Membership and permissions
- Billing and subscriptions
- User presence tracking
- WebSocket signaling for voice setup
- License validation coordination
- Rate limiting
- Moderation logic

**Key Endpoints**:
```
POST   /api/v1/auth/register
POST   /api/v1/auth/login
GET    /api/v1/users/me
GET    /api/v1/servers
POST   /api/v1/servers
GET    /api/v1/channels/:id
WS     /api/v1/ws (signaling)
```

**Database Schema** (Current):
- **users** (id, email, username, password_hash, age_confirmed, e2ee_preference, created_at, updated_at)
- **public_keys** (id, user_id, public_key, key_version, created_at, updated_at)
- **refresh_tokens** (id, user_id, token_hash, user_agent, ip_address, created_at, expires_at)
- **servers** (id, name, owner_id, icon_url, created_at, updated_at) ✅
- **channels** (id, server_id, name, type, emoji, description, position, created_at) ✅
- **server_members** (server_id, user_id, role, joined_at) ✅
- **messages** (id, channel_id, user_id, content, is_encrypted, edited_at, created_at) ✅

### Media Plane (Node.js + mediasoup)

**Port**: 3000
**RTC Ports**: 40000-49999 (UDP)

**Tech Stack**:
- Node.js 20+
- mediasoup (WebRTC SFU)
- Socket.IO (signaling)
- Redis (state sharing for multi-instance)

**Responsibilities**:
- WebRTC media routing (SFU architecture)
- ICE/STUN/TURN handling
- Audio forwarding (no mixing/transcoding)
- Room management (mediasoup routers)
- Transport lifecycle management
- Producer/Consumer management

**Architecture**:
```
mediasoup Worker Pool (1 per CPU core)
    ↓
Router (1 per voice channel/room)
    ↓
WebRTC Transports (2 per user: send + recv)
    ↓
Producers (outgoing audio) / Consumers (incoming audio)
```

**Socket.IO Events**:
- `join-room` - User joins voice channel
- `create-transport` - Create WebRTC transport
- `connect-transport` - Connect transport via DTLS
- `produce` - Start sending audio
- `consume` - Start receiving audio from producer

### Licensing Authority (Go)

**Port**: 8082

**Tech Stack**:
- Go 1.21+
- PostgreSQL (license records)
- Ed25519 cryptographic signing

**Responsibilities**:
- License generation (signed JWTs)
- License validation
- Periodic check-ins from self-hosted instances
- Revocation management
- Usage tracking

**License Structure** (JWT):
```json
{
  "license_id": "uuid",
  "customer_id": "uuid",
  "features": ["high-bitrate", "federation", "api-access"],
  "tier": "enterprise",
  "issued_at": "2024-01-01T00:00:00Z",
  "expires_at": "2025-01-01T00:00:00Z"
}
```

**Validation Flow**:
1. Self-hosted instance boots
2. Validates license signature locally (offline-capable)
3. Periodic check-in every 24-72 hours
4. Licensing authority validates and responds
5. If revoked, instance enters restricted mode (grace period)

## Data Flow Examples

### User Authentication

```
Client → POST /api/v1/auth/login
         ↓
    Control Plane
         ↓
    Check password (bcrypt)
         ↓
    Generate JWT token
         ↓
    Store session in Redis
         ↓
    Return token to client
```

### Sending a Chat Message (Real-Time) ✅

```
SEND:  User types → ChatView → useMessaging (optimistic add) → WebSocket → Server persists
       → Server broadcasts to OTHER users (ExcludeUser) → their useWebSocket → addMessage()
       Sender sees message instantly via optimistic add. Others see it via WS broadcast.

EDIT:  User edits → ChatView → REST PATCH → Server updates DB → updateMessage() locally
       → Server broadcasts message_update → other users' useWebSocket → updateMessage()

DELETE: User deletes → ChatView → REST DELETE → Server deletes → deleteMessage() locally
        → Server broadcasts message_delete → other users' useWebSocket → deleteMessage()

HISTORY: Channel selected → ChatView fetches REST GET → setMessages() → MessageList renders
```

### Joining a Voice Channel

```
1. Client → POST /api/v1/channels/:id/join (HTTP)
           ↓
      Control Plane validates permissions
           ↓
      Returns: { allowed: true, media_server_url }

2. Client → WS connect to Media Plane
           ↓
      emit('join-room', { roomId, userId })
           ↓
      Media Plane creates Router (if needed)
           ↓
      Returns router RTP capabilities

3. Client creates WebRTC transports
   - Send transport (for microphone)
   - Recv transport (for speakers)

4. Client starts producing audio
   - Create Producer on send transport
   - Media plane forwards to other users

5. Other clients consume audio
   - Create Consumer for each Producer in room
   - Receive forwarded audio streams
```

### Voice Room Data Flow

```
User A (Speaking)
  Microphone → Opus Encode → WebRTC Send Transport
                                      ↓
                              Media Plane (SFU)
                             Router forwards packets
                                      ↓
                    ┌─────────────────┴──────────────┐
                    ↓                                ↓
            WebRTC Recv Transport          WebRTC Recv Transport
            Opus Decode → Speakers         Opus Decode → Speakers
                User B                            User C
```

No transcoding - audio packets are forwarded as-is (low latency, high quality).

## Deployment Models

### SaaS (Cloud-Hosted)

```
Users → Cloudflare/CDN → Load Balancer
                              ↓
                    ┌─────────┴─────────┐
                    ↓                   ↓
            Control Plane Pods    Media Plane Pods
            (Kubernetes)          (Kubernetes)
                    ↓                   ↓
                 Managed DB         Redis Cluster
              (RDS/Cloud SQL)
```

**Infrastructure**:
- Kubernetes (EKS/GKE/AKS)
- Managed PostgreSQL with read replicas
- Redis Cluster for HA
- Regional deployment for low latency
- Auto-scaling based on load

### Self-Hosted (Docker Compose)

```
Single Server or VM
┌──────────────────────────────┐
│  Docker Compose Stack        │
│  ┌────────────────────────┐  │
│  │ Control Plane          │  │
│  ├────────────────────────┤  │
│  │ Media Plane            │  │
│  ├────────────────────────┤  │
│  │ PostgreSQL             │  │
│  ├────────────────────────┤  │
│  │ Redis                  │  │
│  └────────────────────────┘  │
└──────────────────────────────┘
```

**Requirements**:
- 4+ CPU cores
- 8+ GB RAM
- 50+ GB storage
- Public IP for WebRTC
- UDP ports 40000-49999 open

### Self-Hosted (Kubernetes)

For larger self-hosted deployments:
- Helm charts for easy deployment
- Horizontal pod autoscaling
- Persistent volume claims for data
- Ingress for HTTPS termination
- Cert-manager for TLS

## Security Architecture

### Authentication & Authorization

**Current Implementation (Phase 1A)** ✅:
- Email + password with Argon2id hashing
- Username validation with profanity filtering and leetspeak detection
- Username format: `username@concordvoice.chat` (federated identity)
- JWT access tokens (15-minute expiry)
- Refresh tokens (30-day expiry) in HttpOnly cookies
- Session management (list sessions, revoke sessions)
- Rate limiting (Redis-based, per-IP and per-user)
- E2EE key generation and storage (RSA-OAEP 2048-bit)

**Planned**:
- Magic link (passwordless)
- OAuth2 (Google, GitHub)
- 2FA via TOTP

**Self-Hosted (Phase 3)**:
- LDAP integration
- SAML 2.0 for enterprise SSO
- Custom identity providers

### Transport Security

- **Signaling**: TLS 1.3 (HTTP/WebSocket)
- **Media**: DTLS + SRTP (WebRTC)
- **E2EE**: Client-side encryption implemented ✅
  - RSA-OAEP 2048-bit for key exchange
  - AES-GCM for message encryption
  - PBKDF2 with 600,000 iterations for key derivation
  - Public keys stored in PostgreSQL, private keys never leave client
  - E2EE → E2EE: Fully encrypted (maximum privacy)
  - E2EE → Non-E2EE: Messages sent in plaintext (user warned)

### Data Privacy

- Minimal telemetry collection
- No biometric ID requirements
- No voice content retention
- Opt-in analytics only
- Clear data deletion policies
- GDPR/CCPA compliance (planned)
- Privacy-first architecture from day one

## Scalability Strategy

### Horizontal Scaling

**Control Plane**:
- Stateless (except Redis sessions)
- Scale behind load balancer
- Database connection pooling
- Read replicas for queries

**Media Plane**:
- Independent scaling from control plane
- Route users to nearest region
- Redis for cross-instance state
- Room sharding for large channels (100+ users)

**Database**:
- Read replicas
- Connection pooling (PgBouncer)
- Separate DBs for domains:
  - User/auth DB
  - Billing DB
  - Server/channel DB
  - Analytics DB (TimescaleDB)

### Geographic Distribution

```
Global Users
    ↓
Geographic DNS routing
    ↓
┌───────┬───────────┬────────┐
↓       ↓           ↓        ↓
US-East US-West   EU-Central APAC
(Media) (Media)   (Media)    (Media)
    ↓       ↓           ↓        ↓
    Central Control Plane (replicated)
    ↓
    Primary Database (with regional replicas)
```

## Performance Targets

### Latency
- **HTTP API**: < 100ms p99
- **WebSocket signaling**: < 50ms p99
- **Voice (same region)**: < 50ms end-to-end
- **Voice (cross-region)**: < 150ms end-to-end

### Capacity (per media plane instance)
- **Concurrent connections**: 1000+ transports
- **Active rooms**: 50+ routers
- **Total users in voice**: 2000+

### Database
- **Query time**: < 10ms p95
- **Connection pool**: 25 max, 5 idle

## Monitoring & Observability

### Metrics (Prometheus)

**Control Plane**:
- Request rate, latency, errors
- Active users, sessions
- Database query performance
- JWT validation rate

**Media Plane**:
- Active workers, routers, transports
- CPU per worker
- Network I/O (UDP traffic)
- Room sizes, concurrent users

### Logging (Loki + Grafana)

- Structured JSON logs in production
- Correlation IDs for request tracing
- Error tracking with stack traces

### Tracing (Jaeger)

- Distributed traces across services
- WebRTC setup flow tracing
- Database query tracing

## Disaster Recovery

### Backup Strategy
- **Database**: Daily snapshots + continuous WAL archiving
- **Redis**: RDB snapshots (optional, ephemeral data)
- **License keys**: Encrypted backup in secure storage

### Recovery Procedures
- Database restore from snapshot
- Recreate Redis (sessions lost, users re-login)
- Redeploy services from Docker images

## Technology Decisions Summary

| Component | Technology | Rationale |
|-----------|-----------|-----------|
| Client Desktop | Electron + React + TS | Proven, fast development, mature WebRTC |
| Client Web | React + TS | Code sharing with desktop |
| Control Plane | Go + Gin | Fast, concurrent, easy to deploy |
| Media Plane | Node.js + mediasoup | Best WebRTC SFU library |
| Database | PostgreSQL | Relational data, JSON support, mature |
| Cache | Redis | Fast, pub/sub, sessions |
| Auth | JWT | Stateless, widely supported |
| Deployment | Kubernetes + Docker | Standard, portable, scalable |

## Next Steps

See [development.md](./development.md) for local development setup and [self-hosting.md](./self-hosting.md) for deployment instructions.
