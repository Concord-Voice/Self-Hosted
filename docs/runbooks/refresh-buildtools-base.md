# Buildtools Base Image Refresh Runbook

> **Status:** Active | **Owner:** Mark Rogers | **Last updated:** 2026-10-16
> **Related issue:** internal development reference omitted
> **Related rules:** `.claude/rules/electron.md` (supply-chain hardening, custom base images), `.claude/rules/media-plane.md` (custom base image discipline)

## 1. Purpose & Scope

This runbook governs the paired refresh of both immutable Node bases consumed by `services/media-plane/Dockerfile`: `ghcr.io/concord-voice/node-buildtools:<node-tag>-<YYYY-MM-DD>` in the builder stage and the upstream `node:<tag>@sha256:<digest>` index in the final runtime stage. `<node-tag>` is not a constant you maintain: the publish workflow derives it from the `FROM node:<tag>` line in `infrastructure/docker/buildtools/Dockerfile`, so bumping that parent (§10) carries the published tag with it. It was hardcoded `24-alpine` until #3172 moved the parent to `node:26-alpine` and left the tag naming the wrong major — harmless at build time, because consumers pin by digest, and misleading to every operator reading this runbook. The custom builder pre-bakes the apk toolchain (`python3 py3-pip make g++ linux-headers`) needed by mediasoup's native worker compile. The runtime remains toolchain-free. Treat both consumer pins as one refresh unit so a deploy rebuild cannot reuse a stale floating runtime base after the builder has moved.

> **Namespace history:** the image namespace migrated from `ghcr.io/markdrogersjr/node-buildtools` to `ghcr.io/concord-voice/node-buildtools` in PR #1348 (Phase 1 of the org migration epic #448). The first org-namespaced image is published post-merge via `workflow_dispatch` on the updated `publish-buildtools-base.yml`. The consumer (`services/media-plane/Dockerfile`) and the `build.yml` age-guard lint flip to the new namespace in a paired follow-up PR. Both old and new namespaces remain readable. Only the publisher target moved.

**In scope:**

- Quarterly refreshes (alpine package updates, security patches)
- Out-of-band refreshes triggered by CVE events
- The first publish (post-merge of #1166 — produced by the first `workflow_dispatch` run of `publish-buildtools-base.yml`. The same workflow path used for every subsequent refresh)
- Revalidating both the builder and final-runtime manifest digests, and updating each changed pin in the same consumer PR
- Rollback when a refresh breaks the downstream consumer

**Out of scope:**

- Automating the consumer bump inside the publisher workflow — the digest changes remain a separately reviewed PR
- SBOM generation, cosign signing — future-work items, separate runbook when introduced

## 2. When to Refresh

| Trigger | Action |
| --- | --- |
| Quarterly calendar (every 90 days) | Dispatch the publish workflow; bump the consumer to the new digest |
| Critical CVE in `python3`, `g++`, `make`, `linux-headers`, or the buildtools parent (currently `node:26-alpine`) | Refresh within 7-day SLO; label PR `risk: security` |
| Alpine major-line version transition (e.g., 3.21 → 3.22) | Refresh at next quarterly window; verify no breaking changes in apk packages |
| CI `buildtools-age-check` warning fires (image >100 days old) | Refresh ASAP — the warning indicates the 90-day cadence has slipped |

Do NOT refresh speculatively. Each refresh is a supply-chain trust delegation. The operational cost of "let's stay current" is non-trivial. The 10-day grace between the 90-day cadence target and the 100-day warning trigger absorbs schedule slip.

## 3. Pre-Flight Checks

Before dispatching the publish workflow, confirm:

1. **Capture the parent tag once; every command below reuses it.** Do not retype the Node major anywhere in this runbook — derive it, so a future bump cannot leave the procedure inspecting an image the build no longer uses:

   ```bash
   NODE_TAG=$(sed -nE 's|^FROM node:([^@[:space:]]+).*|\1|p' \
     infrastructure/docker/buildtools/Dockerfile | head -1)
   echo "${NODE_TAG:?could not parse the parent tag from the buildtools Dockerfile}"
   # -> 26-alpine
   ```

   This is the same expression the publish workflow uses to build the tag prefix, so what you inspect and what it publishes cannot disagree.

2. **The parent pin is current — refresh it BEFORE dispatching, never after.** The parent is digest-pinned, so the publish builds from whatever digest is committed *now*. If upstream has advanced and you dispatch first, you publish an image built from the stale digest, and §5's comparison against the freshly-resolved runtime index then mandates STOP — after the push has already happened, with a junk tag occupying that date. Re-resolve per §10 and land any change before §4.
3. **No in-flight breaking change in the consumer.** Check `services/media-plane/Dockerfile` for any recent edits that depend on toolchain version specifics. If there's an in-flight PR touching the builder stage, coordinate with its author.
4. **Alpine version is current.** `node:${NODE_TAG}` tracks alpine major lines automatically. Verify on Docker Hub that no critical advisories are outstanding for the current line.
5. **No known broken packages.** Check the alpine-linux Bugtraq feed and CVE-tracking sources for advisories on the four apk packages we install.
6. **`infrastructure/docker/buildtools/Dockerfile` matches expected content.** No unauthorized edits — `git log -- infrastructure/docker/buildtools/Dockerfile` should show only authorized commits.

If any check fails — STOP, surface to security-team triage, do not proceed with the refresh.

### Docker server prerequisite

Registry operations require Docker **Server ≥29.8.2**, including manual pulls and
builds in this runbook. CVE-2026-92543 lets an affected Engine classify a registry
as insecure after a malicious mixed-address DNS response; a digest pin does not
protect registry credentials from that path. This floor also retains the earlier
loopback/firewalld protection, including CVE-2025-54388. Compose v2 ≥2.24.5 remains
the self-host requirement; this change does not alter it.

Both `publish` and `smoke-arm64` run `scripts/ci/require-docker-engine.sh` after
trusted checkout and before registry login, QEMU, Buildx setup or any other
possible Engine pull. The guard checks the server in the identity/context used
by later Docker commands. Missing or failed queries, invalid version responses
and servers below 29.8.2 stop the job before registry credentials are supplied.

The hosted amd64 publish job installs Docker CE from Docker's signed Ubuntu
repository before the guard; it uses the fixed `ubuntu-24.04` image label so
the apt suite remains available during `ubuntu-latest` migrations. The guard
then checks the same user/daemon context used by login, Buildx and smoke. A
failed install or a server still below 29.8.2 stops before registry login.
The first internal development reference omitted
stopped at that guard on the preinstalled daemon, before any image push.
The ephemeral native arm64 job installs Docker CE from Docker's official signed
Ubuntu apt repository, checks daemon startup, then runs the guard as root before
root Docker login. This installation is confined to that runner. Its existing
10-minute timeout is unchanged. In the successful 2026-10-29 run below, the
native arm64 job completed in 57 seconds.

The internal development reference omitted
ran from source SHA `804786a1a32784b6a82ebdbc1703063ef955daef` after #4139
merged. Both jobs installed Docker CE and passed the guard on Docker Server
29.9.2 before GHCR login. The publisher built from the pinned upstream index
`sha256:c6ad9e9c1d80f6eb607c26b4b0a132ff7a2344fad1c78ea3baa2a7d4c8de6d94`
on amd64 and arm64, pushed the buildtools index
`sha256:5ef22fe3c3cc81b7df6163ac6bd5a149424b230e7225c59186702a1ce9867b4a`,
and passed both the hosted amd64 and native arm64 toolchain smokes. The run
proves the publisher path exercised here; it does not establish fleet
remediation. Production/staging Docker server inventories and applicable
Blacksmith local-Engine and remote-BuildKit inventories remain outstanding.
Any affected managed-host upgrade needs separate operational authorization.
Historical inventory rows below remain receipts for their original runs.

For #3692's admission prerequisite, `REVIEW.md` permits publisher runtime
validation after merge only with explicit security/deployment acceptance for the
exact PR head and passing static, hermetic actual-step and required CI checks.
The 2026-10-29 run above exercised the real hosted-amd64 upgrade, native-arm64
installation and publisher network behavior after those pre-merge checks. This
runbook publishes from reviewed protected `main`; the workflow itself has no
code-enforced exact-main refusal. Separate publication authorization and the
refresh preflights still apply to future runs.

At each future authorized main publication, record the run URL/source SHA, both
jobs' sanitized server versions, guard ordering before registry/Engine effects,
hosted-amd64 CE upgrade, native-arm64 CE startup and root guard, the unchanged
10-minute smoke outcome, manifest digest and both native smoke results. Retain
failed/refused run evidence; an old daemon's safe refusal does not prove
successful publication. This publisher receipt implies no guard bypass, other
hosted-amd64 upgrade, managed-host change or live cutover. Keep #3692 open
until its separate fleet criteria are evidenced.

## 4. Dispatch the Publish Workflow

Once pre-flight passes, dispatch the workflow:

```bash
REASON="Q<N> <YYYY> quarterly refresh"
# OR for a CVE event:
REASON="CVE-<YYYY>-<NNNNN> in <package>"

RUN_URL="$(gh api --method POST \
  repos/Concord-Voice/Self-Hosted/actions/workflows/publish-buildtools-base.yml/dispatches \
  -F ref=main \
  -F return_run_details=true \
  -f "inputs[reason]=$REASON" \
  --jq .html_url)"
test -n "$RUN_URL"
RUN_ID="${RUN_URL##*/}"
case "$RUN_ID" in ''|*[!0-9]*) echo "invalid workflow run URL: $RUN_URL" >&2; exit 1 ;; esac
```

Watch the run:

```bash
gh run watch "$RUN_ID" --exit-status
```

Expected steps (in order):

1. `Checkout` — repo clones onto the runner
2. `Install official Docker CE on the amd64 runner` — upgrades the ephemeral daemon from Docker's signed Ubuntu apt repository
3. Verify Docker Server ≥29.8.2 with the shared guard before any registry login or helper-image pull
4. `Compute date tag` — reads the parent tag from the buildtools `FROM node:<tag>` line and emits `<node-tag>-YYYY-MM-DD` (today's UTC date); it fails the run rather than publishing an unprefixed tag if that line cannot be parsed
5. `Log in to GHCR` — authenticates via `GITHUB_TOKEN` (no PAT needed)
6. Set up QEMU and Buildx — configures the multi-arch builder
7. `Build and push (multi-arch)` — builds amd64+arm64 manifest, pushes to GHCR
8. `Smoke-test toolchain present` — pulls + execs the new image, verifies python3/g++/make/pip print versions
9. `Surface digest in workflow summary` — Markdown digest block emitted to the GHA UI

If any step fails, the workflow exits non-zero. Common failure modes:

| Failure | Likely cause | Fix |
| --- | --- | --- |
| Docker CE installation fails (step 2) | Apt repository, package or daemon startup failure | Stop publication and inspect the failed step; do not bypass the install or guard. |
| Docker server guard refuses (step 3 or native arm64 guard) | Old, unavailable or invalid daemon response | Stop publication; restore a compliant runner through its approved maintenance path. Do not bypass the guard or substitute the client/remote BuildKit version. |
| GHCR auth fails (step 5) | `GITHUB_TOKEN` scope issue | Verify `permissions: packages: write` is set on the publish job |
| Build push hangs >25 min | GHA runner network slow | Re-dispatch; the timeout-minutes is 30 |
| Smoke-test step exits non-zero | Missing apk package in the Dockerfile | Inspect the Dockerfile against the spec; fix and re-dispatch |
| Smoke-test step exits with `cannot overwrite digest` | Surfaced in workflow run internal development reference omitted (first org-namespace publish, 2026-06-04). The implicit-pull approach attempted in PR #1243 did NOT actually resolve the issue — `docker run --platform linux/arm64 image@digest` under QEMU emulation still triggers the docker daemon's manifest re-import, which fails because the manifest-list digest is already taken. | (Resolved by #1359.) The arm64 smoke step now runs on an org-provisioned native arm64 runner (`runs-on: ubuntu-latest-arm64`, see ADR-0013 §10) rather than QEMU emulation, eliminating cross-platform manifest handling entirely. If you see this error on a future run, verify the `smoke-arm64` job is still pinned to a native arm64 runner and has not regressed to QEMU on amd64; if the runner is unallocated, re-provision per ADR-0013 §10 or fall back to the digest→tag QEMU approach. |
| Digest output empty | `docker/build-push-action` step output not exported | Confirm the workflow file has `id: push` on the build step and `${{ steps.push.outputs.digest }}` in the summary step |

## 5. Capture the Digest

Open `$RUN_URL` in the GitHub UI. Click the `publish` job. Scroll to the "Summary" panel — the Markdown digest block looks like:

> ## Published buildtools base image
>
> **Tag:** `ghcr.io/concord-voice/node-buildtools:<node-tag>-<YYYY-MM-DD>`
>
> **Digest (SHA-pin handle):**
>
> ```
> sha256:abcdef1234567890...
> ```
>
> **Full SHA-pin reference for downstream FROM:**
>
> ```dockerfile
> FROM ghcr.io/concord-voice/node-buildtools:<node-tag>-<YYYY-MM-DD>@sha256:abcdef1234567890...
> ```

Copy the `FROM` line verbatim. This goes into:

- The PR body of the consumer-bumping PR (see §6)
- The runbook's §11 Inventory section (append to the historical table)

Then resolve the upstream multi-architecture runtime index used by the final stage:

```bash
docker buildx imagetools inspect "node:${NODE_TAG}" --format '{{json .Manifest}}'
```

Copy the top-level `digest` value for the Dockerfile pin. Confirm the manifest list contains both `linux/amd64` and `linux/arm64/v8`, and copy those two platform-manifest digests for smoke testing only:

```bash
gh run view <publisher-run-id> --log \
  | rg "FROM docker.io/library/node:${NODE_TAG}@sha256:"
docker run --rm --platform linux/amd64 node@sha256:<runtime-amd64-manifest-digest> \
  sh -c 'node --version && cat /etc/alpine-release'
docker run --rm --platform linux/arm64 node@sha256:<runtime-arm64-manifest-digest> \
  sh -c 'node --version && cat /etc/alpine-release'
```

The publisher log MUST show that the buildtools image resolved the same top-level upstream digest. Both runtime commands MUST report the same patched Node version and Alpine line as the published buildtools image. Use the platform-specific child digests for these smokes because sequential QEMU pulls of one manifest-list digest can hit the `cannot overwrite digest` failure documented in §4. Only the top-level multi-platform index digest belongs in the Dockerfile. If the publisher digest differs, either platform is missing, or the versions differ, STOP rather than opening the consumer PR. Record the reviewed runtime index digest in the PR body and §11 Inventory alongside the buildtools digest.

## 6. Revalidate the Consumer Dockerfile Pins

In a new feature branch (e.g., `feature/<N>-bump-buildtools-<date>`):

1. Open `services/media-plane/Dockerfile`.
2. In the builder stage, replace the existing `FROM` line with the buildtools digest-pinned line copied from §5:

   ```dockerfile
   # BEFORE (the pre-#1169 state this migration replaced — your file will
   # already show a buildtools FROM from the previous refresh):
   FROM node:24-alpine AS builder
   RUN apk add --no-cache python3 py3-pip make g++ linux-headers

   # AFTER:
   FROM ghcr.io/concord-voice/node-buildtools:<node-tag>-<YYYY-MM-DD>@sha256:abcdef... AS builder
   # apk add removed — toolchain pre-baked in base
   ```

3. In the final runtime stage, compare the current `FROM` line with the multi-architecture index digest captured in §5. Update it when the reviewed upstream digest changed:

   ```dockerfile
   FROM node:<NODE_TAG>@sha256:<runtime-index-digest>
   ```

   The runtime digest is mandatory and is revalidated in the same PR as the builder digest. If it is unchanged, do not churn the pin. Never substitute an amd64- or arm64-specific child manifest in the Dockerfile.

4. Run the local builder and final-image smokes:

   ```bash
   docker buildx build services/media-plane --target builder --tag local/media-plane-builder:test
   docker buildx build services/media-plane --load --tag local/media-plane:test
   docker run --rm local/media-plane:test sh -c '! command -v g++ && ! command -v python3 && ! command -v make'
   docker run --rm local/media-plane:test node -e "require('mediasoup').createWorker().then(w => w.close())"
   ```

5. Open the PR with `Closes #<bump-issue>` and both digest lines in the body.

6. The cache-cold `production-compose-docker` Tier-2 CI job (in `build.yml`, post-#1439 consolidation) exercises both new digests at PR-time — the digest-bump PR touches `services/media-plane/Dockerfile`, which matches the job's `production-docker` path gate. The PR description should capture the actual GHA-side build-time and layer-change impact.

## 7. Rollback Procedure

If the consumer build breaks after bumping to a new digest:

1. **Never delete the previously-published image from GHCR.** Manifest digests are forever — that's the point of SHA-pinning.
2. **Revert the consumer Dockerfile** to the previous known-good digest:

   ```bash
   git revert <bump-commit-sha>
   ```

3. **File a follow-up issue** with the failure symptom + the digest that broke + the digest that worked.
4. **Investigate locally** with `docker pull <broken-digest>` + manual exec to triage the breakage.
5. **Do NOT re-dispatch the publish workflow** to "fix" the broken image — that creates yet another digest. The broken digest is a snapshot of a problem. Investigating it is more useful than burying it.

## 8. CVE Response Procedure

For critical CVEs in `python3`, `g++`, `make`, `linux-headers`, or the buildtools parent (currently `node:26-alpine`):

1. Confirm the CVE applies to the alpine version we are tracking (check the alpine secfixes feed).
2. Within 7 days of CVE disclosure:
   - Dispatch the publish workflow with `reason="CVE-<YYYY>-<NNNNN> in <package>"`
   - Revalidate both consumer Dockerfile pins per §6 and update each changed digest
   - Land the bump PR with `risk: security` label
3. Update the runbook §11 Inventory with the new digest + a note linking the CVE.

## 9. Quarterly Cadence Checklist

Add to your calendar: every 90 days from the last refresh, run this checklist:

- [ ] §3 Pre-flight checks (captures `NODE_TAG`; sends you to §10)
- [ ] §10 Re-resolve the publisher parent pin — **before** the dispatch, since the publish builds from the committed digest
- [ ] §4 Dispatch the publish workflow
- [ ] §5 Capture the buildtools and runtime index digests
- [ ] §6 Revalidate both consumer Dockerfile pins and update each changed digest in a new PR
- [ ] §11 Update the inventory

## 10. Refresh the Publisher Parent Pin (run BEFORE §4)

The `infrastructure/docker/buildtools/Dockerfile` parent **is digest-pinned** — `FROM node:<tag>@sha256:…`, pinned by #3283 to satisfy Scorecard's `PinnedDependencies`. This section used to describe adding that pin as optional defense-in-depth; the decision has been taken, so what remains is keeping it current. Re-resolve the digest at each refresh and update it when upstream has moved.

**Ordering is load-bearing, which is why §3 item 2 sends you here first.** The publish builds from the committed digest, so this runs *before* the §4 dispatch. Numbered 10 only because it is reference material an operator consults rather than a step they read in sequence.

**Changing this line changes the published tag.** The publish workflow derives the tag's leading component from it, so a parent moved to `node:27-alpine` publishes `27-alpine-<DATE>` with no other edit — and a line it cannot parse fails the publish loudly rather than pushing an unprefixed tag. The date suffix stays the trailing component, which is what the CI age guard parses.

> **⚠ Keep BOTH the tag and the digest: `node:<tag>@sha256:…`.**
> `docker inspect --format '{{index .RepoDigests 0}}'` prints the digest-only form
> `node@sha256:…`, with **no tag**. Pasting that output verbatim drops the tag, and
> the publish workflow then has no prefix to derive — it refuses the run rather than
> pushing an unprefixed tag, so this fails safe, but it fails. Compose the two halves
> by hand as shown in step 2.

1. Pull the current parent and capture its digest (`NODE_TAG` from §3 item 1):

   ```bash
   docker pull "node:${NODE_TAG}"
   docker inspect "node:${NODE_TAG}" --format '{{index .RepoDigests 0}}'
   # Example output: node@sha256:1234...   <- digest only; the tag is NOT in this output
   ```

2. Edit `infrastructure/docker/buildtools/Dockerfile`, re-attaching the tag:

   ```dockerfile
   FROM node:<NODE_TAG>@sha256:1234...
   ```

   `<NODE_TAG>` is the value from §3 item 1 — **write that, not a remembered major.** This
   line is the publisher's parent: typing a stale one here downgrades the image that the
   next dispatch builds, and it also moves the published tag's prefix, since the workflow
   derives that prefix from this very line.

3. **Land this before dispatching §4.** If the digest did not move, there is nothing to commit and you proceed straight to §4. If it did, the publish must be built from the new one — committing it "alongside the next bump PR" would publish from the old digest and strand the run at §5.

**Scope:** this controls the publisher input, not the final media-plane runtime stage. The final runtime pin in §6 is independently mandatory because it controls deploy-time rebuild provenance; the two are separate pins that happen to resolve to the same upstream index today.

## 11. Inventory

Append one row per published image. The most recent entry is the canonical current pin.

| Date | Reason | Buildtools tag | Buildtools digest | Runtime index digest | Consumer PR |
| --- | --- | --- | --- | --- | --- |
| 2026-05-28 | First publish (post-merge of #1166) — dispatched during the `/dev-lifecycle 1169` session that produced the consumer-bump PR | `24-alpine-2026-05-28` | `sha256:a53963b772b7a3e09d1d82bf4fac9fd775350a45d94430527bdf76f8729a3fc2` | Floating tag (historical) | internal development reference omitted (consumer-bump) |
| 2026-06-04 | internal development reference omitted for the GHCR migration; publish succeeded before the historical arm64 smoke failure | `24-alpine-2026-06-04` | `sha256:27bf28e3754c513fedf6c5dcee5ba22949c5276a206d31d779bc304024c10eda` | Floating tag (historical) | internal development reference omitted (consumer-bump) |
| 2026-06-11 | internal development reference omitted after #1459 | `24-alpine-2026-06-11` | `sha256:c7eb5b536b378b8985bd0e535b661215d6adff42b596adfe25b4e94a04842c3a` | Not consumed | Not consumed |
| 2026-07-13 | internal development reference omitted for #1922 | `24-alpine-2026-07-13` | `sha256:7eb1a5270a928bfc9be0bc92d0aa1c086fd6776095025ddd3a063510e936eee4` | `sha256:a0b9bf06e4e6193cf7a0f58816cc935ff8c2a908f81e6f1a95432d679c54fbfd` | internal development reference omitted (consumer-bump) |
| 2026-09-04 | internal development reference omitted | `24-alpine-2026-09-04` | `sha256:f94d703b688d942e89e6225977c0110560a9f2b5095377d209aca10e4cf5df2a` | `sha256:e67514e5d0f6c46656005e1b693b2ec9d52e80b641307de684d4a015ba7a4eaf` | internal development reference omitted (consumer-bump) |
| 2026-10-07 | internal development reference omitted | `26-alpine-2026-10-07` | `sha256:163883defd34c5afeaad2eca717d6198793ab74f4de2e1e40785dadebade1e26` | `sha256:0b36e8c136b94cd4fcf02188228e76c31ad5872eef3fec8cbd2eee500cfd9e80` | internal development reference omitted (consumer-bump) |
| 2026-10-29 | internal development reference omitted | `26-alpine-2026-10-29` | `sha256:5ef22fe3c3cc81b7df6163ac6bd5a149424b230e7225c59186702a1ce9867b4a` | `sha256:c6ad9e9c1d80f6eb607c26b4b0a132ff7a2344fad1c78ea3baa2a7d4c8de6d94` | internal development reference omitted (consumer-bump) |

(Append one row per future refresh.)

## 12. Cross-References

- Supply-chain rule: `.claude/rules/electron.md` § "Supply-chain hardening"
- Media-plane rule: `.claude/rules/media-plane.md` § "Custom base image discipline"
- Sibling supply-chain runbook: `docs/runbooks/upgrade-sha-pinned-action.md`
- ADR-0006 (Cache Tier Split): `docs/adr/0006-cache-tier-split.md`
- Investigation report (parent #1111): `docs/superpowers/reports/2026-05-22-1111-media-plane-docker-investigation.md` §5
