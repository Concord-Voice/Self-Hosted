# Security Policy

## Supported Versions

Currently supported versions for security updates:

| Version | Supported          |
| ------- | ------------------ |
| 0.1.x   | :white_check_mark: |
| < 0.1   | :x:                |

## Reporting a Vulnerability

We take security seriously. If you discover a security vulnerability, please follow these steps:

### 1. **DO NOT** Create a Public Issue

Public disclosure puts all users at risk. Instead, use one of these private channels:

### 2. Preferred Reporting Methods

**Option A: GitHub Security Advisories (Recommended)**
1. Go to the [Security tab](https://github.com/markdrogersjr/Concord/security/advisories)
2. Click "Report a vulnerability"
3. Fill out the form with details

**Option B: Email**
- Send to: `security@concordvoice.chat`
- Use PGP encryption if possible (key available on request)
- Include "SECURITY" in the subject line

### 3. What to Include

- **Vulnerability type** (SQLi, XSS, auth bypass, etc.)
- **Affected component** (Control Plane, Media Plane, Client)
- **Steps to reproduce** (detailed PoC)
- **Impact assessment** (what can an attacker do?)
- **Suggested fix** (if you have one)
- **Your contact info** (for coordination)

### 4. Response Timeline

- **24 hours**: Initial acknowledgment
- **72 hours**: Preliminary assessment
- **7 days**: Detailed response and timeline
- **90 days**: Public disclosure (coordinated)

### 5. Disclosure Policy

We follow **coordinated disclosure**:
- We work with you to understand and fix the issue
- We credit you in the security advisory (if desired)
- We allow 90 days before public disclosure
- We may request extensions for complex fixes

### 6. Scope

**In Scope:**
- Authentication and authorization
- Cryptography implementations
- Data privacy issues
- Session management
- API vulnerabilities
- Client-side security
- Infrastructure security

**Out of Scope:**
- Social engineering
- Physical security
- Denial of Service (unless critical)
- Issues requiring physical access
- Issues in dependencies (report to upstream)

### 7. Bug Bounty

We currently do not offer a bug bounty program, but we:
- Acknowledge researchers in our Hall of Fame
- Credit you in CVE reports
- Provide swag for significant findings (when available)

### 8. Security Best Practices

When using Concord:
- ✅ Always use HTTPS in production
- ✅ Keep software up to date
- ✅ Use strong passwords (12+ characters)
- ✅ Enable E2EE when possible
- ✅ Review session list regularly
- ✅ Use 2FA (when available)

### 9. Known Security Limitations

**Current Alpha/Beta Status:**
- E2EE implementation is in active development
- Third-party security audits not yet completed
- Use at your own risk in production environments

### 10. Security Updates

Subscribe to security updates:
- Watch this repository for security advisories
- Follow [@ConcordVoice](https://twitter.com/ConcordVoice) (when available)
- Join our major comms platforms security channel

## Security Architecture

### Authentication
- Argon2id password hashing
- JWT access tokens (15 min expiry)
- HttpOnly refresh tokens (30 day expiry)
- Rate limiting on all auth endpoints

### Encryption
- TLS 1.3 for transport
- RSA-OAEP 4096-bit for E2EE
- AES-GCM for key wrapping
- PBKDF2 600k iterations for key derivation

### Infrastructure
- PostgreSQL with row-level security
- Redis with authentication
- Docker container isolation
- Regular dependency updates

## Contact

**Security Team:**
- Email: security@concordvoice.chat
- PGP Key: [Available on request]

**Project Leads:**
- GitHub: @markdrogersjr

---

**Last Updated:** 2026-02-22

Thank you for helping keep Concord secure! 🔒
