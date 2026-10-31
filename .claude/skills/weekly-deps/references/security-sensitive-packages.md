# Security-Sensitive Package Allowlist (shared universal default)

Read this file when Phase 3.5 (confidence assessment) needs to decide whether a bumped package elevates a PR to LOW confidence. (Phase 3.4 — community-signal lookup — produces a separate verdict that also feeds into Phase 3.5.) Any bump (patch / minor / major) of a package matching one of the patterns below sets the confidence to LOW regardless of its tier, because the change touches a code path where a subtle behavior shift can introduce a security regression.

**This list is the shared, universal default — package families that sit on a security boundary in essentially any codebase that uses them** (authentication, cryptography, parsers/validators, deserialization, HTTP clients, XSS sanitizers, logging). It is intentionally framework- and stack-agnostic: it does NOT name any single repo's framework, UI library, or infrastructure-specific actions.

**The effective list = this universal default ∪ the repo's `.weekly_deps.security_sensitive_extra`.** Repos add their own boundary packages (a UI framework, a desktop runtime, a server framework, code-signing actions, etc.) via `.weekly_deps.security_sensitive_extra` in `.claude/repo-config.json` — see `knowledge/repo-config.reference.md`. Do not edit this shared file to add a repo-specific package; put it in that repo's config instead.

## NPM / JavaScript

### Validation & input parsing — error-handling and schema-correctness load-bearing
- `ajv`, `ajv-formats`, `ajv-keywords` (JSON Schema — bumps change validation semantics)
- `joi`, `zod`, `yup`, `class-validator`
- `js-yaml`, `yaml` (YAML parsing — historical CVE surface)
- `xml2js`, `fast-xml-parser` (XML attacks)
- `qs`, `query-string` (HTTP query parsing — prototype pollution surface)

### Authentication & session
- `jose`, `jsonwebtoken`, `jwt-*`
- `passport`, `passport-*`
- `@octokit/auth-*`
- `oauth*`, `openid-client`

### Cryptography
- `argon2`, `@node-rs/argon2`, `bcrypt`, `bcryptjs`
- `crypto-js`, `node-forge`, `forge`
- `tweetnacl`, `libsodium-wrappers`, `sodium-native`
- `hash-wasm`
- `@noble/*`, `@stablelib/*`

### XSS / HTML sanitization
- `dompurify`, `sanitize-html`, `xss`

### Networking & SSRF surface (HTTP clients + websockets)
- `axios`, `node-fetch`, `got`, `undici`, `request`
- `ws`, `socket.io`, `socket.io-client`, `engine.io-client`

### Serialization & deserialization
- `protobufjs`, `protobuf-ts/*`
- `bson`, `cbor`, `@msgpack/msgpack`

### Logging / observability — token-leakage surface
- `winston`, `pino`, `bunyan` (bumps may change error-cause propagation)

## Go

### Authentication & session
- `github.com/golang-jwt/jwt*`
- `golang.org/x/oauth2`
- `github.com/coreos/go-oidc*`
- `github.com/go-webauthn/webauthn`

### Cryptography
- `golang.org/x/crypto` (all sub-modules — argon2, chacha20, etc.)
- `github.com/cloudflare/circl`
- `github.com/google/tink-go`

### Networking & SSRF surface
- `github.com/gorilla/websocket`
- `github.com/nats-io/nats.go`
- `github.com/redis/go-redis*`

### Database — query-parameter handling
- `github.com/lib/pq`, `github.com/jackc/pgx*`
- `github.com/jmoiron/sqlx`

### Validation & input parsing
- `github.com/go-playground/validator*`
- `gopkg.in/yaml.v*`

### Configuration & secret handling
- `github.com/spf13/viper` (config loader — env-var resolution and secret-injection surface)
- `github.com/joho/godotenv` (env-file loader)

## Python

### Authentication & session
- `pyjwt`, `authlib`, `python-jose`, `oauthlib`, `requests-oauthlib`

### Cryptography
- `cryptography`, `pyopenssl`, `pynacl`, `bcrypt`, `argon2-cffi`

### Validation & input parsing / deserialization
- `pydantic`, `marshmallow`, `cerberus`, `jsonschema`
- `pyyaml`, `lxml`, `defusedxml`

### Networking & SSRF surface (HTTP clients)
- `requests`, `httpx`, `urllib3`, `aiohttp`

## GitHub Actions

Any action that touches secrets, fetches code, or executes arbitrary scripts:
- `actions/checkout` (any major bump — fetch-depth semantics, token handling)
- `actions/setup-*` (toolchain provisioning)
- `actions/cache` (cache poisoning surface)
- `actions/download-artifact`, `actions/upload-artifact` (artifact-handling surface)

> Code-signing actions, cloud-credential actions, deploy actions, and any action referenced by a repo's secret-provisioning workflow are **repo-specific** — add them via `.weekly_deps.security_sensitive_extra`, not here.

## How to apply this list

In Phase 3.5 of `weekly-deps` (confidence assessment), after parsing the bumped package name(s) from the PR title:

1. Compute the **effective list** = this universal default ∪ the repo's `.weekly_deps.security_sensitive_extra` (read via `rc_get '.weekly_deps.security_sensitive_extra | join(" ")' ''`).
2. For grouped PRs (`chore: bump the X group ...`), fetch the dependabot summary body via `gh pr view $PR --json body --jq '.body'` and extract individual package names from the bullet list.
3. For each bumped package, glob-match against the effective list (e.g., `ajv*` matches `ajv`, `ajv-formats`, `ajv-keywords`).
4. If ANY bumped package matches → set confidence to LOW regardless of other signals.

This is intentionally biased toward false-positives (LOW confidence on borderline packages). The cost of an unnecessary Copilot review is low (a few minutes of bot time), while the cost of a missed security regression in a validation or auth library is potentially much higher.

## When to extend the effective list

Add a pattern when:
- A new dependency joins a codebase that lives on a security boundary (a new auth library, a new crypto helper, a new validation framework).
- A repo introduces a new defended surface that depends on a specific package family.
- A real-world incident shows that bumps in a package class caused a regression you'd want Copilot review to have caught.

**Where to add it:** if the package is universally security-sensitive (sits on the auth/crypto/parser/deserialization/HTTP-client/XSS/logging boundary in any stack), add it to this shared file. If it's specific to one repo's stack (its UI framework, its desktop runtime, its server framework, its signing actions), add it to that repo's `.weekly_deps.security_sensitive_extra` instead — keep this shared list universal.

Do NOT add a pattern just because a package is "popular" or "frequently used" — the allowlist's value is in being narrow enough that LOW confidence is meaningful. If everything is LOW, nothing is LOW.
