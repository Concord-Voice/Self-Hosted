# Concord Voice Self-Hosted

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

## Run it

See `docs/runbooks/selfhost-public-acceptance.md` and run
`infrastructure/deploy/install-selfhost.sh` on a fresh host.

## Develop

```bash
npm ci && npm test --workspaces          # packages
npm --prefix client/admin test
npm --prefix services/media-plane test
DATABASE_URL=<isolated-db-url> go -C services/control-plane test ./...
```

Go tests truncate every table, so point `DATABASE_URL` at a database you can lose.

## License

Concord Voice Source License 1.0, converting to AGPL-3.0-or-later on
2030-02-15. See `LICENSE`.
