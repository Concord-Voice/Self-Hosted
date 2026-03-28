# Concord Voice Frequently Asked Questions (FAQ)

**Last Updated:** 2026-03-27

This document answers common questions about Concord Voice's licensing, security, development, deployment, and usage.

---

## 📋 Table of Contents

- [Licensing & Legal](#-licensing--legal)
- [Security & Privacy](#-security--privacy)
- [Development & Contributing](#-development--contributing)
- [Deployment & Self-Hosting](#-deployment--self-hosting)
- [Features & Usage](#-features--usage)
- [Troubleshooting](#-troubleshooting)

---

## 📜 Licensing & Legal

### What is BSL 1.1?

The Business Source License (BSL) is a "source-available" license that allows you to:
- ✅ View and audit the source code
- ✅ Use the software for personal, educational, or non-commercial purposes
- ✅ Modify the code for your own use
- ✅ Self-host with unlimited users (non-commercial)

But **restricts** you from:
- ❌ Using it to compete with the official Concord Voice service
- ❌ Offering Concord Voice as a SaaS product to others (without a commercial license)

After **4 years**, each version automatically converts to **AGPL-3.0** (fully open source).

---

### Can I use Concord Voice for free?

**Yes!** You can:
- Use the hosted SaaS version at concordvoice.chat
- Self-host for personal use with unlimited users
- Self-host for non-profit or educational purposes

---

### Can I self-host Concord Voice for my business?

**Yes**, with a commercial self-hosted license. Commercial use requires a paid license.

You **don't need a license** if:
- You're a non-profit organization
- You're using it for educational purposes
- You're using it for personal/family use

---

### Can I modify the code?

**Yes!** You can modify Concord Voice for your own use. However:
- ✅ Modify for personal self-hosted instance
- ✅ Contribute improvements back (encouraged!)
- ❌ Create a competing SaaS service with your modifications
- ❌ Sell modified versions as a service

---

### Can I contribute to Concord Voice?

**Absolutely!** We welcome contributions. When you contribute code:
- Your contributions are licensed under the same BSL 1.1 terms
- After 4 years, they become AGPL-3.0 (fully open source)
- You retain copyright to your original work
- See [.github/CONTRIBUTING.md](./.github/CONTRIBUTING.md) for guidelines

**How to contribute:**
1. Fork the repository
2. Create a feature branch
3. Make your changes with tests
4. Submit a Pull Request
5. Participate in code review

---

### What happens after 4 years?

Each version of Concord Voice becomes **fully open source** under AGPL-3.0 after 4 years:
- Anyone can use it for any purpose (including competing services)
- Modifications must be shared (AGPL copyleft)
- This ensures long-term sustainability and community ownership

**Example timeline:**
- Version 1.0 released January 2025 → becomes AGPL January 2029
- Version 2.0 released June 2026 → becomes AGPL June 2030

---

### Why not just use AGPL or MIT from the start?

We chose BSL 1.1 because it:
- **Protects development investment** - Prevents unfair competition in early years
- **Ensures eventual openness** - Guarantees full open source after 4 years
- **Allows transparency** - Source code is available for audit from day one
- **Supports sustainability** - Enables us to build a business while staying open

---

### Can I fork Concord Voice?

**Yes**, with restrictions:
- ✅ Fork for personal use
- ✅ Fork to contribute improvements
- ✅ Fork to self-host for your organization
- ❌ Fork to create a competing SaaS service (until 4 years pass)

After 4 years, older versions become AGPL-3.0 and you can fork them for any purpose.

---

### What's the difference between BSL and "open core"?

**Traditional Open Core:**
- Core features: Open source (e.g., MIT)
- Premium features: Proprietary forever
- Creates permanent split between free and paid

**BSL 1.1 (Concord Voice's approach):**
- All features: Source-available immediately
- All code: Becomes fully open source after 4 years
- No permanent proprietary components

We believe this is **more open** long-term than traditional open core models.

---

### Do I need a license to self-host for my family?

**No!** Personal, family, educational, and small non-commercial use is always free:
- ✅ Set up a server for your gaming group
- ✅ Use it for your family chat
- ✅ Deploy it for your school or non-profit
- ✅ Run it for your hobby project

**You only need a commercial license if:**
- You're offering Concord Voice as a hosted service to external customers (competing SaaS)
- You're embedding Concord Voice in a product you sell to customers
- Internal team communication (even at a business) is always free to self-host

---

### Can universities and schools use Concord Voice?

**Yes, completely free!** Educational institutions can:
- Self-host with unlimited users
- Modify the code for research
- Use it for teaching purposes
- No license required

We encourage educational use and welcome academic contributions.

---

### What if I want to compete with Concord Voice?

Under BSL 1.1, you **cannot** create a competing SaaS service during the initial 4-year period.

**However:**
- After 4 years, each version becomes AGPL-3.0
- You can then use older versions to compete freely
- You can always build a complementary service (bots, plugins, integrations)

We believe 4 years is a fair window to establish the project while ensuring eventual full openness.

---

### Can I use Concord Voice in my commercial product?

**It depends:**

| Use Case | License Required? |
|----------|-------------------|
| Internal team communication | No (self-host free) |
| Selling Concord Voice as a service | Yes (commercial SaaS prohibited) |
| Embedding in your product (for customers) | Yes (commercial license) |
| Building integrations/bots | No (complementary services allowed) |
| Using the API for your business | No (API usage is open) |

**When in doubt**, email licensing@concordvoice.chat for clarification.

---

### How does BSL 1.1 compare to other licenses?

| License | Source Available? | Can Self-Host? | Can Compete? | Eventually Open? |
|---------|-------------------|----------------|--------------|------------------|
| **BSL 1.1** | ✅ Yes | ✅ Yes (free non-commercial) | ❌ No (4 years) | ✅ Yes (AGPL after 4y) |
| MIT | ✅ Yes | ✅ Yes | ✅ Yes | ✅ Already open |
| AGPL-3.0 | ✅ Yes | ✅ Yes | ✅ Yes (must share changes) | ✅ Already open |
| Proprietary | ❌ No | ❌ No | ❌ No | ❌ No |
| Open Core | ✅ Partial | ⚠️ Partial | ⚠️ Partial | ⚠️ Core only |

BSL 1.1 balances immediate transparency with sustainable development.

---

## 🔒 Security & Privacy

### Is Concord Voice secure?

Yes. We implement industry-standard security practices:

**Cryptography:**
- 🔐 **E2EE:** RSA-OAEP 4096-bit + AES-GCM encryption
- 🔑 **Password Hashing:** Argon2id (OWASP recommended)
- 🎫 **Tokens:** JWT access tokens (15min) + HttpOnly refresh tokens (30d)
- 🔐 **Key Derivation:** PBKDF2 600k iterations
- 🔒 **Transport:** TLS 1.3

**Infrastructure:**
- 🚫 **Rate Limiting:** Redis-based, per-IP and per-user
- 🛡️ **Pre-commit Hooks:** Prevents committing secrets
- **Pre-commit Scanning:** 20 hooks via Python pre-commit framework (TruffleHog, detect-secrets, golangci-lint, ESLint, etc.)
- **CI/CD:** GitHub Actions (build.yml) with parallel test + coverage + SonarQube Quality Gate (mandatory)
- 🗄️ **Database:** PostgreSQL with row-level security
- 🔐 **Redis:** Authentication enabled
- 🐳 **Docker:** Container isolation

---

### What is End-to-End Encryption (E2EE)?

E2EE means your messages are encrypted on your device before being sent. Only you and the recipient can read them—not even Concord Voice can decrypt them.

**How it works:**
1. Your device generates a unique key pair (public + private keys)
2. Your private key is encrypted with your password and never leaves your device unencrypted
3. Messages are encrypted with the recipient's public key
4. Only the recipient's private key can decrypt the message

**Important:** If you forget your password, we **cannot** reset it or recover your encrypted data.

---

### Can Concord Voice read my messages?

**For E2EE messages:** No. Messages are encrypted on your device with the recipient's public key. Concord Voice servers only see encrypted ciphertext.

**For non-E2EE messages:** Yes, technically. If you join a non-E2EE server (self-hosted or opted-out), messages are stored in plaintext and server admins can access them.

**Recommendation:** Always use E2EE for private conversations.

---

### How do I report a security vulnerability?

**DO NOT** create a public GitHub issue.

**Preferred methods:**

1. **GitHub Security Advisories (Recommended)**
   - Go to [Security tab](https://github.com/markdrogersjr/Concord/security/advisories)
   - Click "Report a vulnerability"
   - Fill out the form

2. **Email:** security@concordvoice.chat
   - Use PGP encryption if possible (key available on request)
   - Include "SECURITY" in subject line

**Response timeline:**
- 24 hours: Initial acknowledgment
- 72 hours: Preliminary assessment
- 7 days: Detailed response and timeline
- 90 days: Public disclosure (coordinated)

See [.github/SECURITY.md](./.github/SECURITY.md) for full policy.

---

### What data does Concord Voice collect?

**Hosted SaaS:**
- ✅ Email address (for account recovery)
- ✅ Username
- ✅ Encrypted password (Argon2id hash)
- ✅ E2EE public key
- ✅ Session metadata (IP, user agent, last used)
- ❌ **Not collected:** Biometric data, location, message content (E2EE), call recordings

**Self-Hosted:**
- You control all data
- Concord Voice receives no data from self-hosted instances (except license validation pings)

**Telemetry:**
- Minimal opt-in telemetry for error reporting (planned)
- Always anonymized and aggregated
- Clear opt-out mechanism

---

### Is two-factor authentication (2FA) supported?

**Status:** Planned for Phase 2 (Q3 2026)

**Planned methods:**
- TOTP (Google Authenticator, Authy)
- WebAuthn/FIDO2 (hardware keys like YubiKey)
- Backup codes

For now, use a strong unique password (12+ characters) and password manager.

---

### What about vendor lock-in?

Concord Voice is designed to **prevent vendor lock-in**:
- ✅ Full source code available (audit anytime)
- ✅ Self-host option (data portability)
- ✅ Standard protocols (WebRTC, PostgreSQL, Redis)
- ✅ Export your data anytime (feature planned)
- ✅ Becomes fully open source after 4 years

If we shut down or change direction, you can always self-host.

---

## 👨‍💻 Development & Contributing

### How do I set up the development environment?

**Quick start:**

```bash
# 1. Clone the repository
git clone https://github.com/markdrogersjr/Concord.git
cd Concord

# 2. Install git hooks (prevents committing secrets)
./scripts/install-git-hooks.sh

# 3. Start everything (Docker + services + client)
./scripts/dev-start.sh

# That's it! The app will open in Electron
```

See [docs/GETTING_STARTED.md](./docs/GETTING_STARTED.md) for detailed instructions.

---

### What are the prerequisites for development?

**Required:**
- **Node.js** 20+
- **Go** 1.24+
- **Docker** & **Docker Compose**
- **Git**

**Optional:**
- **PostgreSQL** client (psql) for database inspection
- **Redis** CLI for cache debugging

---

### What's the project structure?

```
Concord/
├── client/desktop/          # Electron desktop application
├── services/
│   ├── control-plane/       # Go backend (auth, channels, billing)
│   ├── media-plane/         # Node.js WebRTC SFU (voice/video)
│   └── licensing-authority/ # Go licensing service (planned)
├── docs/                    # Documentation
├── scripts/                 # Development scripts
└── infrastructure/docker/   # Docker Compose configs
```

See [README.md](./README.md) for full breakdown.

---

### What coding standards should I follow?

**Go (Backend):**
- Use `gofmt` and `golangci-lint`
- Follow [Effective Go](https://go.dev/doc/effective_go)
- Handle all errors explicitly
- Add comments for exported functions
- Keep functions small and focused

**TypeScript (Frontend):**
- Use ESLint and Prettier
- Follow Airbnb style guide
- Use TypeScript strict mode
- Prefer interfaces over types
- Avoid `any` type
- Use functional components with hooks

**Commit Messages:**
```bash
<type>: <description>

Types: feat, fix, refactor, docs, test, chore, perf, style

Examples:
git commit -m "feat: add username validation"
git commit -m "fix: resolve token expiration bug"
```

See [.github/CONTRIBUTING.md](./.github/CONTRIBUTING.md) for full guidelines.

---

### How do I run tests?

**Backend tests:**
```bash
cd services/control-plane
go test -v ./...

# With coverage
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

**Frontend tests** (783 tests, 70.28% coverage):
```bash
cd client/desktop
npm test
```

---

### How do I create a Pull Request?

1. **Create a branch:**
   ```bash
   git checkout -b feature/my-feature
   ```

2. **Make changes:**
   - Write code
   - Add tests
   - Update documentation

3. **Test locally:**
   ```bash
   ./scripts/dev-stop.sh
   ./scripts/dev-start.sh --rebuild
   ```

4. **Commit:**
   ```bash
   git add .
   git commit -m "feat: add my feature"
   ```

5. **Push and create PR:**
   ```bash
   git push origin feature/my-feature
   # Then open a Pull Request on GitHub
   ```

6. **PR review:**
   - Respond to all comments
   - Make requested changes
   - Wait for approval
   - Squash and merge

---

### What should I NOT commit?

**Never commit:**
- ❌ Passwords or API keys
- ❌ Private keys (.pem, .key)
- ❌ .env files with secrets
- ❌ Database credentials
- ❌ JWT secrets
- ❌ Large binary files (>10MB)

**Git hooks will catch these**, but be careful.

**Always use:**
- ✅ Environment variables
- ✅ .env.example templates
- ✅ GitHub Secrets for CI/CD

---

## 🚀 Deployment & Self-Hosting

### How do I self-host Concord Voice?

**Status:** Self-hosting documentation coming in Phase 3 (Q4 2026).

**Current workaround** (for developers):
```bash
# Use Docker Compose for local deployment (from project root)
docker-compose up -d

# Or use the dev script for the full stack
./scripts/dev-start.sh

# Access at http://localhost:8080
```

Full self-hosting guide with single-command setup coming soon.

---

### What are the system requirements for self-hosting?

**Minimum (for testing):**
- 2 CPU cores
- 4 GB RAM
- 20 GB disk space
- Docker + Docker Compose

**Recommended (for production):**
- 4 CPU cores
- 8 GB RAM
- 100 GB SSD
- PostgreSQL 16
- Redis 7
- Reverse proxy with TLS (nginx, Caddy)

**For voice (Media Plane):**
- Add 2 CPU cores per 25 concurrent voice users
- Add 1 GB RAM per 25 concurrent voice users

---

### Can I run Concord Voice on Raspberry Pi?

**Maybe** - depends on usage:
- ✅ **Raspberry Pi 4 (4GB+)**: Should work for small groups (<10 users)
- ⚠️ **Raspberry Pi 3**: Marginal, text-only might work
- ❌ **Voice chat**: Not recommended on Pi due to CPU requirements

We haven't officially tested Pi deployment, but community reports welcome!

---

### What about Kubernetes/cloud deployment?

**Phase 4** (Q1-Q2 2027) will include:
- Kubernetes manifests
- Helm charts
- Multi-region deployment guides
- Cloud provider templates (AWS, GCP, Azure)

For now, Docker Compose is the supported method.

---

## 🎯 Features & Usage

### What platforms are supported?

**Current:**
- ✅ **Desktop:** Electron app (Windows, macOS, Linux)
- 🚧 **Web:** Planned (Q3 2026)
- 📋 **Mobile:** Planned (Q1 2027)

---

### Can I use Concord on mobile?

**Not yet.** Mobile apps (iOS + Android) are planned for Phase 4 (Q1 2027).

The desktop Electron app works on laptops/tablets, but no native mobile UI yet.

---

### How is Concord different from major comms platforms?

| Feature | major comms platforms | Concord |
|---------|---------|---------|
| **E2EE** | ❌ | ✅ Yes |
| **Self-Hosting** | ❌ | ✅ Yes (Phase 3) |
| **Source Code** | ❌ Proprietary | ✅ BSL 1.1 (→ AGPL) |
| **Privacy** | ⚠️ Collects data | ✅ Minimal telemetry |
| **Business Model** | Nitro subscriptions | À-la-carte + self-host |
| **Voice Quality** | Good (Opus) | ✅ Hi-Fi (256kbps+, 7 quality tiers) |
| **Bots** | ✅ Yes | 📋 Planned (Phase 5) |

**Concord Voice's focus:** Privacy, sovereignty, eventual open source.

---

### Does Concord support screen sharing?

**Status:** ✅ Implemented (Phase 1C, PR #114).

Screen sharing uses WebRTC via the mediasoup SFU. Features include:
- Full-screen and window/tab selection via ScreenSharePicker
- AV1 codec for screen share (high quality at low bitrate)
- IGNIS decoder budget system for managing stream limits
- Opt-in screen share viewing to conserve bandwidth

---

### Can I upload files?

**Status:** Planned for Phase 2 (Q3 2026).

Will support:
- Image uploads
- Video uploads
- General file uploads (with size limits by tier)
- CDN integration for fast delivery

---

### Is there a file size limit?

**Planned limits (Phase 2):**
- **Free tier:** 10 MB per file, 1 GB per month
- **Premium:** 100 MB per file, 10 GB per month
- **Server Premium:** Unlimited for server members

---

### Can I create bots?

**Status:** Planned for Phase 5 (Q3-Q4 2027).

Will include:
- Bot user type
- Public API with webhooks
- Bot permission system
- Bot directory/marketplace

For now, focus is on core chat and voice features.

---

## 🛠️ Troubleshooting

### The dev environment won't start

**Check:**
1. Docker is running: `docker ps`
2. Ports are free: `lsof -ti:8080` (should be empty)
3. Logs: `tail -f logs/control-plane.log`

**Fix:**
```bash
# Stop everything cleanly
./scripts/dev-stop.sh

# Start fresh
./scripts/dev-start.sh --rebuild
```

---

### I'm getting "port already in use" errors

**Find and kill the process:**
```bash
# For port 8080 (control-plane)
lsof -ti:8080 | xargs kill -9

# For port 5173 (Vite dev server)
lsof -ti:5173 | xargs kill -9

# Then restart
./scripts/dev-start.sh
```

---

### Database migrations aren't running

The migration system uses `golang-migrate/v4` and runs automatically on server startup. There are currently **47 migrations** (000001-000047).

**Check migration status:**
```bash
cd services/control-plane
make migrate-version
```

**Run migrations manually:**
```bash
cd services/control-plane
make migrate-up
```

**Rollback:**
```bash
cd services/control-plane
make migrate-down
```

**If migrations fail**, ensure PostgreSQL is running and accessible:
```bash
docker exec -it concord-postgres psql -U concord -d concord -c "SELECT version FROM schema_migrations;"
```

---

### WebSocket connection fails

**Check:**
1. Control Plane is running: `curl http://localhost:8080/health`
2. JWT token is valid (not expired)
3. Browser console for errors

WebSocket support is fully implemented with ticket-based authentication. Ensure you obtain a ticket via `POST /api/v1/auth/ws-ticket` before connecting.

---

### Tests are failing

**For Go tests:**
```bash
cd services/control-plane

# Run with verbose output
go test -v ./...

# Check for race conditions
go test -race ./...
```

**For frontend tests:**
```bash
cd client/desktop

# Clear cache and retry
rm -rf node_modules package-lock.json
npm install
npm test
```

---

### How do I reset my local database?

**WARNING:** This deletes all local data.

```bash
# Stop services
./scripts/dev-stop.sh

# Remove Docker volumes (from project root)
docker-compose down -v

# Restart
./scripts/dev-start.sh
```

---

### The Electron app won't open

**Check:**
1. Vite dev server is running: `curl http://localhost:5173`
2. Node modules installed: `cd client/desktop && npm install`
3. Console output: Look for errors in terminal

**Rebuild:**
```bash
cd client/desktop
npm run build
npm run dev
```

---

### Where can I get help?

**Resources:**
- **Documentation:** [docs/](./docs/)
- **GitHub Discussions:** [Ask questions](https://github.com/markdrogersjr/Concord/discussions)
- **GitHub Issues:** [Report bugs](https://github.com/markdrogersjr/Concord/issues)
- **Security:** security@concordvoice.chat
- **Licensing:** licensing@concordvoice.chat
- **General:** (major comms platforms server coming soon)

---

## 📚 Additional Resources

- **README:** [README.md](./README.md) - Project overview
- **TODO:** [[internal]](.[internal]) - Current priorities
- **ROADMAP:** [ROADMAP.md](./ROADMAP.md) - Long-term plan
- **Contributing:** [.github/CONTRIBUTING.md](./.github/CONTRIBUTING.md) - Development guidelines
- **Security:** [.github/SECURITY.md](./.github/SECURITY.md) - Security policy
- **Architecture:** [docs/architecture.md](./docs/architecture.md) - System design
- **Getting Started:** [docs/GETTING_STARTED.md](./docs/GETTING_STARTED.md) - Setup guide
- **AI Context:** [[internal]](.[internal]) - For AI assistants

---

## 💬 Still Have Questions?

If your question isn't answered here:

1. **Search GitHub Issues:** Your question may have been asked before
2. **Check Documentation:** Browse the [docs/](./docs/) folder
3. **Ask on Discussions:** Start a conversation on GitHub Discussions
4. **Email Us:**
   - General: (coming soon)
   - Licensing: licensing@concordvoice.chat
   - Security: security@concordvoice.chat

---

**Last Updated:** 2026-03-03 | **Version:** 1.3.0
