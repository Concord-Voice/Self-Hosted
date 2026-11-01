# Concord Voice Self-Hosted

<a href="https://sonarcloud.io/summary/new_code?id=Concord-Voice_Self-Hosted"><img align="right" src="https://sonarcloud.io/api/project_badges/quality_gate?project=Concord-Voice_Self-Hosted" alt="Quality gate" /></a>

[![Lines of Code](https://sonarcloud.io/api/project_badges/measure?project=Concord-Voice_Self-Hosted&metric=ncloc)](https://sonarcloud.io/summary/new_code?id=Concord-Voice_Self-Hosted)
[![Coverage](https://sonarcloud.io/api/project_badges/measure?project=Concord-Voice_Self-Hosted&metric=coverage)](https://sonarcloud.io/summary/new_code?id=Concord-Voice_Self-Hosted)
[![Security Rating](https://sonarcloud.io/api/project_badges/measure?project=Concord-Voice_Self-Hosted&metric=security_rating)](https://sonarcloud.io/summary/new_code?id=Concord-Voice_Self-Hosted)
[![Reliability Rating](https://sonarcloud.io/api/project_badges/measure?project=Concord-Voice_Self-Hosted&metric=reliability_rating)](https://sonarcloud.io/summary/new_code?id=Concord-Voice_Self-Hosted)
[![Maintainability Rating](https://sonarcloud.io/api/project_badges/measure?project=Concord-Voice_Self-Hosted&metric=sqale_rating)](https://sonarcloud.io/summary/new_code?id=Concord-Voice_Self-Hosted)
[![Nightly CI](https://github.com/Concord-Voice/Self-Hosted/actions/workflows/nightly-ci.yml/badge.svg)](https://github.com/Concord-Voice/Self-Hosted/actions/workflows/nightly-ci.yml)

[![AI Code Assurance](https://sonarcloud.io/api/project_badges/ai_code_assurance?project=Concord-Voice_Self-Hosted)](https://sonarcloud.io/summary/new_code?id=Concord-Voice_Self-Hosted)
[![Dependabot](https://img.shields.io/badge/Dependabot-enabled-success?logo=dependabot)](./.github/dependabot.yml)
[![license](https://img.shields.io/badge/license-CVSL%201.0-blue)](./LICENSE)
[![security](https://img.shields.io/badge/security-policy-green)](./.github/SECURITY.md)
[![E2EE](https://img.shields.io/badge/E2EE-AES--256--GCM%20%2F%20RSA--OAEP%204096-success)](./docs/architecture.md)
[![self-hosted](https://img.shields.io/badge/self--hosted-supported-blue)](./infrastructure/deploy)
[![SonarQube Cloud](https://sonarcloud.io/images/project_badges/sonarcloud-dark.svg)](https://sonarcloud.io/summary/new_code?id=Concord-Voice_Self-Hosted)

Run the Concord Voice server on your own hardware.

**Website:** [www.concordvoice.com](https://www.concordvoice.com) | **Wiki:** [Self-Hosted wiki](https://github.com/Concord-Voice/Self-Hosted/wiki) | **Security:** security@concordvoice.com

---

## Introduction

Concord Voice is a privacy-first place to talk with your friends, your community, or your team. It gives you voice calls, video, screen sharing, and chat. Your device encrypts each message before it leaves, so our servers store your messages only as scrambled data that we cannot read. You can use our hosted service, or you can run the same software on your own hardware.

This repository holds the Concord Voice server. You can install it on a machine that you control.
Your community then talks on your hardware, and your data stays with you.

It is the same server code that runs our hosted service. We do not keep a better version behind a paywall.
One installer sets up the full stack with Docker, and a small wrapper command starts, stops, and checks it.

For guides and background, see the [Self-Hosted wiki](https://github.com/Concord-Voice/Self-Hosted/wiki).

---

## Technical overview

The Concord Voice server stack: the Go control plane (`services/control-plane`),
the mediasoup media plane (`services/media-plane`), the admin portal
(`client/admin`), the self-host installer and wrapper (`infrastructure/deploy`),
and Compose files to run it on your own hardware.

It also holds the shared client packages:

- `packages/protocol` (`@concord-voice/protocol`): the WebSocket event schemas
  and the media policy that clients and the media plane share.
- `packages/e2ee-client` (`@concord-voice/e2ee-client`): the client-side
  end-to-end encryption engine.

Neither package is published yet.

### Run it

See `docs/runbooks/selfhost-public-acceptance.md` and run
`infrastructure/deploy/install-selfhost.sh` on a fresh host.

### Develop

```bash
npm ci && npm test --workspaces          # packages
npm --prefix client/admin test
npm --prefix services/media-plane test
DATABASE_URL=<isolated-db-url> go -C services/control-plane test ./...
```

Go tests truncate every table, so point `DATABASE_URL` at a database you can lose.

---

## Security

The server stores messages only in encrypted form. It cannot read message content, encrypted attachments, or voice and video. It can read profile images such as avatars and banners.

Report a vulnerability by email to security@concordvoice.com. Do not open a public issue for it. The [security policy](./.github/SECURITY.md) describes the disclosure process.

---

## Contributing

We welcome help. Read the [contributing guide](./.github/CONTRIBUTING.md) and the [code of conduct](./.github/CODE_OF_CONDUCT.md) before you open a pull request. Report bugs and request features in our [feedback repository](https://github.com/Concord-Voice/Concord-Voice/issues).

---

## License

This repository uses the Concord Voice Source License 1.0 (CVSL 1.0). Anyone can read the source. Personal and non-profit use is free. Any use by a for-profit company or a government body needs a commercial license. Each version converts to AGPL-3.0-or-later on 2030-02-15 at the latest.

See [LICENSE](./LICENSE) for the full terms.

---

## Contact

- **Website:** [www.concordvoice.com](https://www.concordvoice.com)
- **Security:** security@concordvoice.com
- **Privacy:** privacy@concordvoice.com
- **General:** contact-us@concordvoice.com
