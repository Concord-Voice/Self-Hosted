# ZAP API DAST — finding anchor (#1465)

This file exists so that ZAP's dynamic-scan findings can be ingested by
**GitHub Code Scanning**, and to explain why they all point here.

## Why every `zap-api` alert points at this file

GitHub Code Scanning anchors every alert to a **file in the repository
checkout** — its location model is `(file path, line)`. ZAP is a DAST tool:
it reports finding locations as the **URL it scanned**
(`http://localhost:8080/api/v1/...`), which has no corresponding source
file. Uploading ZAP's SARIF unmodified is rejected with:

> SARIF URI scheme "http" did not match the checkout URI scheme "file"

So [`sarif-postprocess.py`](sarif-postprocess.py) rewrites each finding's
physical location to **this file** (with `uriBaseId: %SRCROOT%`, the
repo-relative shape Code Scanning expects) and prepends the **real scanned
URL** to the alert message. The original URL is also preserved in the
finding's `properties.originalUri`.

## Reading a `zap-api` alert

- The alert **title/message** begins with the actual scanned target, e.g.
  `[http://localhost:8080/api/v1/servers] ...`.
- The alert is **anchored to line 1 of this file** — that anchor is a
  technical necessity, not a claim that the issue lives here.
- The finding is **advisory** (not in the #1431 Alpha Code Scanning Gate).

## Scope

REST control-plane surface only — scanned against the **ephemeral
`CONCORD_ENV=test` compose stack**, never production. The WebSocket hub,
media-plane Socket.IO/WebRTC, and NATS are out of scope. See
[`pr.yaml`](pr.yaml) / [`full.yaml`](full.yaml) and the workflow header
in [`../.github/workflows/zap-api-dast.yml`](../.github/workflows/zap-api-dast.yml).
