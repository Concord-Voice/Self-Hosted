# Security

> **Reporting a vulnerability?** Follow the disclosure policy in
> [`.github/SECURITY.md`](.github/SECURITY.md). Do not open a public issue.

## Threat model summary

Self-Hosted is the server an operator runs: a Go control plane, a Node media plane, an admin
console, a root host agent and Docker Compose files. The operator is the trust root and the
data controller. End-to-end encryption keeps stored content from a database copy, a backup
or a storage vendor; it does not protect users from an operator who modifies the server.
The highest-value targets are host root (through the host agent and the update path), the
shared token-signing secret, and the account key hierarchy. The thinnest ground comes first
in the model: one shared secret roots user and service identity, and content confidentiality
rests on the server it is meant to exclude. After those come the service-to-service routes,
the service bus, first-run setup and secrets kept as plain files.

Full model: [`docs/threat-model.md`](docs/threat-model.md).

## Guidance for automated review

This section is review guidance for people, AI agents and scanners. It is not the
disclosure scope; the disclosure policy linked above defines that.

**Report**

- A route that grants access without the check its group requires: user token, admin
  session, or service proof.
- Authority or data that crosses a community server, DM or account boundary.
- A path from request input to a host-agent command, a shell, SQL or a file path.
- A secret, token, key or message content that reaches a log, a response or a URL.
- A change that publishes a new port, widens proxy trust, or weakens a compose hardening
  setting.
- A change to update, release or image pinning that removes a verification step.

**Do not report**

- That the operator, or host root, can read metadata or stop the service. That is the trust
  model.
- Findings in development-only files (`docker-compose.dev.yml`, the `tools` profile) unless
  they reach a production start.
- Missing rate limits on routes that already sit behind a stricter shared limit.
- Dependency advisories with no reachable call path; the dependency scanners own those.

**Severity**

- Critical: host root or other remote code execution; token forgery or another
  authentication bypass, the admin console included; account takeover without the victim's
  action; loss of content confidentiality for users other than the attacker.
- High: privilege escalation inside one community server; an authorization failure across
  a community server, DM or account boundary that exposes no plaintext; access to another
  user's data that needs the victim's action.
- Medium: metadata disclosure, denial of service by one client, audit gaps.
- Low: hardening and defence-in-depth without a demonstrated path.
