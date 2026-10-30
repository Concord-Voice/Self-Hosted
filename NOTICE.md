# NOTICE — Third-Party Software Used by Concord Voice

**Generated:** 2026-10-30
**MinIO distribution addendum updated:** 2026-09-27
**Admin portal family added:** 2026-08-28
**Concord Voice License:** [Concord Voice Source License 1.0 (CVSL 1.0)](./LICENSE) → AGPL-3.0-or-later on 2030-02-15
**Audit reference:** [docs/legal/dependency-license-audit.md](docs/legal/dependency-license-audit.md)

This NOTICE file lists the third-party open-source software used by Concord
Voice and acknowledges the copyright holders of that software, as required by
each component's license. Concord Voice LLC gratefully acknowledges the
contributions of these projects to the Concord Voice platform.

## Summary

Concord Voice distributes 212 third-party software components across its three
CVSL application artifact families:

- **61** Go modules linked into the control-plane runtime binaries. The image
  ships **three** Go binaries — `main` (`./cmd/server`), `migrate-media` and
  `mfa-rekey` (`services/control-plane/Dockerfile:39-41,57-59`) — and the table
  is the union of what all three link. The image also carries the admin portal
  bundle below, so it is not Go-only.
- **145** npm packages in the media-plane production bundle
- **6** npm packages in the admin portal production bundle (`client/admin`),
  compiled by the `admin-ui-builder` stage and copied into the control-plane
  runtime image at `/admin-ui` (`services/control-plane/Dockerfile:8,66`)

(Counts exclude Concord Voice's own private packages — `@concordvoice/media-plane`
and `@concordvoice/admin` — and the local stub override
`node-domexception-stub`. Those are first-party components, not third-party
attributions. The separately distributed MinIO server image described below is
also excluded from this linked/bundled application dependency count.)

The desktop application — Electron, Chromium, its npm dependencies and the
shared `packages/*` libraries it bundles — is distributed from the
[Desktop repository](https://github.com/Concord-Voice/Desktop), whose NOTICE.md
attributes it.

Application-dependency license distribution:

| License family | Share | Compatible with CVSL 1.0 + AGPL-3.0 |
|---|---|---|
| MIT | 76.9% (163) | ✓ |
| Apache-2.0 | 7.5% (16) | ✓ |
| BSD-3-Clause | 6.6% (14) | ✓ |
| ISC | 4.2% (9) | ✓ |
| BSD-2-Clause | 2.4% (5) | ✓ |
| BlueOak-1.0.0 | 1.9% (4) | ✓ |
| Unlicense | 0.5% (1) | ✓ |
| MPL-2.0 (build-time only) | not in the count above | ✓ |
| CC-BY-3.0 / CC-BY-4.0 (build-data only) | not in the count above | ✓ (attribution required) |

Shares are exact counts over the 212 components tabulated below, not estimates.
The final two rows cover build-time and build-data dependencies, which are not
part of that 212 and therefore carry no share of it; see
[docs/legal/dependency-license-audit.md](docs/legal/dependency-license-audit.md)
for their treatment.

No GPL, AGPL, SSPL, BUSL, Elastic License, Commons Clause, or other strong
copyleft / non-commercial dependency is linked into or bundled with the three
CVSL application artifact families counted above. Concord's separately
distributed MinIO server image is an AGPL-3.0-or-later upstream program and is
recorded separately below; it is not part of the 212-component count.

## License Texts

The license texts for each license family are available at the canonical
SPDX URLs:

- **MIT** — [https://opensource.org/license/mit](https://opensource.org/license/mit)
- **ISC** — [https://opensource.org/license/isc](https://opensource.org/license/isc)
- **Apache-2.0** — [https://www.apache.org/licenses/LICENSE-2.0](https://www.apache.org/licenses/LICENSE-2.0)
- **BSD-2-Clause** — [https://opensource.org/license/bsd-2-clause](https://opensource.org/license/bsd-2-clause)
- **BSD-3-Clause** — [https://opensource.org/license/bsd-3-clause](https://opensource.org/license/bsd-3-clause)
- **0BSD** — [https://opensource.org/license/0bsd](https://opensource.org/license/0bsd)
- **MPL-2.0** — [https://www.mozilla.org/en-US/MPL/2.0/](https://www.mozilla.org/en-US/MPL/2.0/)
- **AGPL-3.0-or-later** — [https://www.gnu.org/licenses/agpl-3.0.html](https://www.gnu.org/licenses/agpl-3.0.html)
- **BlueOak-1.0.0** — [https://blueoakcouncil.org/license/1.0.0](https://blueoakcouncil.org/license/1.0.0)
- **Unlicense** — [https://unlicense.org/](https://unlicense.org/)
- **Python-2.0** — [https://docs.python.org/3/license.html](https://docs.python.org/3/license.html)
- **CC0-1.0** — [https://creativecommons.org/publicdomain/zero/1.0/](https://creativecommons.org/publicdomain/zero/1.0/)
- **CC-BY-3.0** — [https://creativecommons.org/licenses/by/3.0/](https://creativecommons.org/licenses/by/3.0/)
- **CC-BY-4.0** — [https://creativecommons.org/licenses/by/4.0/](https://creativecommons.org/licenses/by/4.0/)

Individual project copyright notices and full license texts are preserved
in the redistributed source / binary form (npm `node_modules`, Go module
cache). For
projects that ship their own NOTICE file (Apache-2.0 § 4 requirement),
that content is included with the project in its `node_modules` directory
or Go module cache and is not duplicated here.

**Exception — the admin portal bundle.** Only the compiled `dist` output is
copied out of the `admin-ui-builder` stage, so that family's `node_modules`
(and the license texts inside it) stay in the builder stage and never reach the
runtime image. Attribution for those four packages therefore rests on the
Admin Portal table in this file and the canonical SPDX URLs above, not on
shipped `node_modules`.

## Separately Distributed MinIO Server

Concord distributes a standalone MinIO server derived from the upstream release
below. The derivative applies a bounded dependency patch to `go.mod` and
`go.sum`; it is not linked into a Concord application binary and is not
relicensed under CVSL 1.0.

| Item | Recorded value |
| --- | --- |
| Upstream release | `RELEASE.2025-10-15T17-29-55Z` |
| Upstream commit | `9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a` |
| Derivative commit | `823f9aa2bd624e7fa0bcc2217c4174d9ed6e1c8a` |
| Upstream license | AGPL-3.0-or-later |
| Runtime package | `ghcr.io/concord-voice/minio:RELEASE.2025-10-15T17-29-55Z.CONCORD.823f9aa2bd62` |
| Corresponding-source package | `ghcr.io/concord-voice/minio-source:RELEASE.2025-10-15T17-29-55Z.CONCORD.823f9aa2bd62` |

The corresponding-source package contains the derivative source archive and
checksum, the dependency patch and checksum, `go.mod` and `go.sum` checksums,
source provenance, the runtime-manifest digest, Concord's Dockerfile, and the
reproducible build recipe. The upstream tag and commit remain unchanged; the
derivative commit records only the bounded manifest patch. See the
[MinIO refresh runbook](docs/runbooks/refresh-minio-image.md) for verified
release identities. The public build materials are maintained at
[Concord-Voice/Concord-Voice `infrastructure/docker/minio`](https://github.com/Concord-Voice/Concord-Voice/tree/main/infrastructure/docker/minio).
The runtime image preserves upstream `LICENSE`, `NOTICE`, and `CREDITS`
unchanged under `/licenses`.

Immutable runtime and source digests are captured by the trusted publisher run;
they are not represented by mutable placeholders in this NOTICE. On initial
package creation, both packages remain private until Concord records legal
approval of the attribution, corresponding-source contents, and public package
presentation. Once public, GHCR package visibility cannot return to private, so
later MinIO releases require that recorded legal approval before dispatch. This
section is an engineering distribution record, not a legal conclusion about
compliance or aggregation.

## Go Control-Plane Runtime Dependencies

The following Go modules are linked into the control-plane runtime binaries
(services/control-plane). The set is the **union across all three shipped
binaries** — `./cmd/server`, `./cmd/migrate-media` and `./cmd/mfa-rekey` — not
the server alone; a module reachable from any of them is distributed and owes an
attribution row. The other two currently link strict subsets of the server's
set, so the union equals the server's today, but that is a fact about the
current tree rather than a guarantee.

| Module | Version | License |
|---|---|---|
| github.com/boombuler/barcode | v1.1.0 | MIT |
| github.com/cespare/xxhash/v2 | v2.3.0 | MIT |
| github.com/coreos/go-oidc/v3 | v3.21.0 | Apache-2.0 |
| github.com/dustin/go-humanize | v1.1.0 | MIT |
| github.com/fxamacker/cbor/v2 | v2.9.6 | MIT |
| github.com/gabriel-vasile/mimetype | v1.4.16 | MIT |
| github.com/gin-contrib/sse | v1.1.2 | MIT |
| github.com/gin-gonic/gin | v1.12.0 | MIT |
| github.com/go-jose/go-jose/v4 | v4.1.6 | Apache-2.0 |
| github.com/go-playground/locales | v0.14.2 | MIT |
| github.com/go-playground/universal-translator | v0.18.2 | MIT |
| github.com/go-playground/validator/v10 | v10.30.5 | MIT |
| github.com/go-viper/mapstructure/v2 | v2.5.0 | MIT |
| github.com/go-webauthn/webauthn | v0.18.2 | BSD-3-Clause |
| github.com/go-webauthn/x | v0.3.1 | BSD-3-Clause |
| github.com/goccy/go-yaml | v1.19.2 | MIT |
| github.com/golang-jwt/jwt/v5 | v5.3.1 | MIT |
| github.com/golang-migrate/migrate/v4 | v4.20.1 | MIT |
| github.com/google/go-tpm | v0.9.8 | Apache-2.0 |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause |
| github.com/gorilla/websocket | v1.5.3 | BSD-2-Clause |
| github.com/joho/godotenv | v1.5.1 | MIT |
| github.com/klauspost/compress | v1.20.3 | MIT |
| github.com/klauspost/cpuid/v2 | v2.4.1 | MIT |
| github.com/klauspost/crc32 | v1.3.0 | BSD-3-Clause |
| github.com/leodido/go-urn | v1.5.0 | MIT |
| github.com/lib/pq | v1.12.3 | MIT |
| github.com/mattn/go-isatty | v0.0.24 | MIT |
| github.com/minio/crc64nvme | v1.1.1 | Apache-2.0 |
| github.com/minio/md5-simd | v1.1.2 | Apache-2.0 |
| github.com/minio/minio-go/v7 | v7.4.1 | Apache-2.0 |
| github.com/nats-io/nats.go | v1.54.0 | Apache-2.0 |
| github.com/nats-io/nkeys | v0.4.16 | Apache-2.0 |
| github.com/nats-io/nuid | v1.0.1 | Apache-2.0 |
| github.com/pelletier/go-toml/v2 | v2.4.3 | MIT |
| github.com/philhofer/fwd | v1.2.1 | MIT |
| github.com/pquerna/otp | v1.5.0 | Apache-2.0 |
| github.com/quic-go/qpack | v0.6.0 | MIT |
| github.com/quic-go/quic-go | v0.64.1 | MIT |
| github.com/redis/go-redis/v9 | v9.23.0 | BSD-2-Clause |
| github.com/rs/xid | v1.6.0 | MIT |
| github.com/shopspring/decimal | v1.4.0 | MIT |
| github.com/stripe/stripe-go/v87 | v87.0.0 | MIT |
| github.com/tinylib/msgp | v1.6.5 | MIT |
| github.com/ugorji/go/codec | v1.3.2 | MIT |
| github.com/vmihailenco/msgpack/v5 | v5.4.1 | BSD-2-Clause |
| github.com/vmihailenco/tagparser/v2 | v2.0.0 | BSD-2-Clause |
| github.com/x448/float16 | v0.8.4 | MIT |
| github.com/zeebo/xxh3 | v1.1.0 | BSD-2-Clause |
| go.mongodb.org/mongo-driver/v2 | v2.9.2 | Apache-2.0 |
| go.uber.org/atomic | v1.12.0 | MIT |
| go.yaml.in/yaml/v3 | v3.0.5 | MIT |
| golang.org/x/crypto | v0.58.0 | BSD-3-Clause |
| golang.org/x/image | v0.47.0 | BSD-3-Clause |
| golang.org/x/net | v0.62.0 | BSD-3-Clause |
| golang.org/x/oauth2 | v0.37.0 | BSD-3-Clause |
| golang.org/x/sync | v0.24.0 | BSD-3-Clause |
| golang.org/x/sys | v0.49.0 | BSD-3-Clause |
| golang.org/x/text | v0.43.0 | BSD-3-Clause |
| google.golang.org/protobuf | v1.36.12 | BSD-3-Clause |
| gopkg.in/ini.v1 | v1.67.3 | Apache-2.0 |

---

## Media-Plane Production Dependencies

The following npm packages are included in the media-plane production
bundle (services/media-plane):

| Package | Version | License | Repository |
|---|---|---|---|
| @colors/colors | 1.6.0 | MIT | https://github.com/DABH/colors.js |
| @dabh/diagnostics | 2.0.8 | MIT | https://github.com/DABH/diagnostics |
| @isaacs/fs-minipass | 4.0.1 | ISC | https://github.com/npm/fs-minipass |
| @redis/bloom | 6.3.0 | MIT | https://github.com/redis/node-redis |
| @redis/client | 6.3.0 | MIT | https://github.com/redis/node-redis |
| @redis/json | 6.3.0 | MIT | https://github.com/redis/node-redis |
| @redis/search | 6.3.0 | MIT | https://github.com/redis/node-redis |
| @redis/time-series | 6.3.0 | MIT | https://github.com/redis/node-redis |
| @so-ric/colorspace | 1.1.6 | MIT | https://github.com/so-ric/colorspace |
| @socket.io/component-emitter | 3.1.2 | MIT | https://github.com/socketio/emitter |
| @types/cors | 2.8.19 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| @types/node | 26.6.5 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| @types/triple-beam | 1.3.5 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| @types/ws | 8.18.1 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| accepts | 1.3.8 | MIT | https://github.com/jshttp/accepts |
| accepts | 2.0.0 | MIT | https://github.com/jshttp/accepts |
| async | 3.2.6 | MIT | https://github.com/caolan/async |
| body-parser | 2.4.0 | MIT | https://github.com/expressjs/body-parser |
| buffer-equal-constant-time | 1.0.1 | BSD-3-Clause | https://github.com/goinstant/buffer-equal-constant-time |
| bytes | 3.1.2 | MIT | https://github.com/visionmedia/bytes.js |
| call-bind-apply-helpers | 1.0.2 | MIT | https://github.com/ljharb/call-bind-apply-helpers |
| call-bound | 1.0.4 | MIT | https://github.com/ljharb/call-bound |
| chownr | 3.0.0 | BlueOak-1.0.0 | https://github.com/isaacs/chownr |
| color-convert | 3.1.3 | MIT | https://github.com/Qix-/color-convert |
| color-name | 2.1.0 | MIT | https://github.com/colorjs/color-name |
| color-string | 2.1.4 | MIT | https://github.com/Qix-/color-string |
| color | 5.0.3 | MIT | https://github.com/Qix-/color |
| content-disposition | 2.0.1 | MIT | https://github.com/jshttp/content-disposition |
| content-type | 2.1.0 | MIT | https://github.com/jshttp/content-type |
| cookie-signature | 1.2.2 | MIT | https://github.com/visionmedia/node-cookie-signature |
| cookie | 0.7.2 | MIT | https://github.com/jshttp/cookie |
| cors | 2.8.6 | MIT | https://github.com/expressjs/cors |
| data-uri-to-buffer | 4.0.1 | MIT | https://github.com/TooTallNate/node-data-uri-to-buffer |
| debug | 4.4.3 | MIT | https://github.com/debug-js/debug |
| depd | 2.0.0 | MIT | https://github.com/dougwilson/nodejs-depd |
| dunder-proto | 1.0.1 | MIT | https://github.com/es-shims/dunder-proto |
| ecdsa-sig-formatter | 1.0.11 | Apache-2.0 | https://github.com/Brightspace/node-ecdsa-sig-formatter |
| ee-first | 1.1.1 | MIT | https://github.com/jonathanong/ee-first |
| enabled | 2.0.0 | MIT | https://github.com/3rd-Eden/enabled |
| encodeurl | 2.0.0 | MIT | https://github.com/pillarjs/encodeurl |
| engine.io-parser | 5.2.3 | MIT | https://github.com/socketio/socket.io |
| engine.io | 6.6.11 | MIT | https://github.com/socketio/socket.io |
| es-define-property | 1.0.1 | MIT | https://github.com/ljharb/es-define-property |
| es-errors | 1.3.0 | MIT | https://github.com/ljharb/es-errors |
| es-object-atoms | 1.1.2 | MIT | https://github.com/ljharb/es-object-atoms |
| escape-html | 1.0.3 | MIT | https://github.com/component/escape-html |
| etag | 1.8.1 | MIT | https://github.com/jshttp/etag |
| express | 5.3.0 | MIT | https://github.com/expressjs/express |
| fecha | 4.2.3 | MIT | git+https://taylorhakes@github.com/taylorhakes/fecha |
| fetch-blob | 3.2.0 | MIT | https://github.com/node-fetch/fetch-blob |
| finalhandler | 2.1.1 | MIT | https://github.com/pillarjs/finalhandler |
| flatbuffers | 25.9.23 | Apache-2.0 | https://github.com/google/flatbuffers |
| fn.name | 1.1.0 | MIT | https://github.com/3rd-Eden/fn.name |
| formdata-polyfill | 4.0.10 | MIT | git+https://jimmywarting@github.com/jimmywarting/FormData |
| forwarded | 0.2.0 | MIT | https://github.com/jshttp/forwarded |
| fresh | 2.0.0 | MIT | https://github.com/jshttp/fresh |
| function-bind | 1.1.2 | MIT | https://github.com/Raynos/function-bind |
| get-intrinsic | 1.3.0 | MIT | https://github.com/ljharb/get-intrinsic |
| get-proto | 1.0.1 | MIT | https://github.com/ljharb/get-proto |
| gopd | 1.2.0 | MIT | https://github.com/ljharb/gopd |
| h264-profile-level-id | 2.3.3 | ISC | https://github.com/versatica/h264-profile-level-id |
| has-symbols | 1.1.0 | MIT | https://github.com/inspect-js/has-symbols |
| hasown | 2.0.4 | MIT | https://github.com/inspect-js/hasOwn |
| http-errors | 2.0.1 | MIT | https://github.com/jshttp/http-errors |
| iconv-lite | 0.7.3 | MIT | https://github.com/pillarjs/iconv-lite |
| inherits | 2.0.4 | ISC | https://github.com/isaacs/inherits |
| ipaddr.js | 1.9.1 | MIT | https://github.com/whitequark/ipaddr.js |
| is-promise | 4.0.0 | MIT | https://github.com/then/is-promise |
| is-stream | 2.0.1 | MIT | https://github.com/sindresorhus/is-stream |
| jsonwebtoken | 9.0.3 | MIT | https://github.com/auth0/node-jsonwebtoken |
| jwa | 2.0.1 | MIT | https://github.com/brianloveswords/node-jwa |
| jws | 4.0.1 | MIT | https://github.com/brianloveswords/node-jws |
| kuler | 2.0.0 | MIT | https://github.com/3rd-Eden/kuler |
| lodash.includes | 4.3.0 | MIT | https://github.com/lodash/lodash |
| lodash.isboolean | 3.0.3 | MIT | https://github.com/lodash/lodash |
| lodash.isinteger | 4.0.4 | MIT | https://github.com/lodash/lodash |
| lodash.isnumber | 3.0.3 | MIT | https://github.com/lodash/lodash |
| lodash.isplainobject | 4.0.6 | MIT | https://github.com/lodash/lodash |
| lodash.isstring | 4.0.1 | MIT | https://github.com/lodash/lodash |
| lodash.once | 4.1.1 | MIT | https://github.com/lodash/lodash |
| logform | 2.7.0 | MIT | https://github.com/winstonjs/logform |
| math-intrinsics | 1.1.0 | MIT | https://github.com/es-shims/math-intrinsics |
| media-typer | 1.1.0 | MIT | https://github.com/jshttp/media-typer |
| mediasoup | 3.31.0 | ISC | https://github.com/versatica/mediasoup |
| merge-descriptors | 2.0.0 | MIT | https://github.com/sindresorhus/merge-descriptors |
| mime-db | 1.52.0 | MIT | https://github.com/jshttp/mime-db |
| mime-db | 1.54.0 | MIT | https://github.com/jshttp/mime-db |
| mime-types | 2.1.35 | MIT | https://github.com/jshttp/mime-types |
| mime-types | 3.0.2 | MIT | https://github.com/jshttp/mime-types |
| minipass | 7.1.3 | BlueOak-1.0.0 | https://github.com/isaacs/minipass |
| minizlib | 3.1.0 | MIT | https://github.com/isaacs/minizlib |
| ms | 2.1.3 | MIT | https://github.com/vercel/ms |
| nats | 2.29.3 | Apache-2.0 | https://github.com/nats-io/nats.node |
| negotiator | 0.6.3 | MIT | https://github.com/jshttp/negotiator |
| negotiator | 1.0.0 | MIT | https://github.com/jshttp/negotiator |
| nkeys.js | 1.1.0 | Apache-2.0 | https://github.com/nats-io/nkeys.js |
| node-fetch | 3.3.2 | MIT | https://github.com/node-fetch/node-fetch |
| object-assign | 4.1.1 | MIT | https://github.com/sindresorhus/object-assign |
| object-inspect | 1.13.4 | MIT | https://github.com/inspect-js/object-inspect |
| on-finished | 2.4.1 | MIT | https://github.com/jshttp/on-finished |
| once | 1.4.0 | ISC | https://github.com/isaacs/once |
| one-time | 1.0.0 | MIT | https://github.com/3rd-Eden/one-time |
| parseurl | 1.3.3 | MIT | https://github.com/pillarjs/parseurl |
| path-to-regexp | 8.4.0 | MIT | https://github.com/pillarjs/path-to-regexp |
| proxy-addr | 2.0.8 | MIT | https://github.com/jshttp/proxy-addr |
| qs | 6.16.1 | BSD-3-Clause | https://github.com/ljharb/qs |
| range-parser | 1.2.1 | MIT | https://github.com/jshttp/range-parser |
| raw-body | 3.0.2 | MIT | https://github.com/stream-utils/raw-body |
| readable-stream | 3.6.2 | MIT | https://github.com/nodejs/readable-stream |
| redis | 6.3.0 | MIT | https://github.com/redis/node-redis |
| router | 2.2.0 | MIT | https://github.com/pillarjs/router |
| safe-buffer | 5.2.1 | MIT | https://github.com/feross/safe-buffer |
| safe-stable-stringify | 2.5.0 | MIT | https://github.com/BridgeAR/safe-stable-stringify |
| safer-buffer | 2.1.2 | MIT | https://github.com/ChALkeR/safer-buffer |
| semver | 7.7.4 | ISC | https://github.com/npm/node-semver |
| send | 1.2.1 | MIT | https://github.com/pillarjs/send |
| serve-static | 2.2.1 | MIT | https://github.com/expressjs/serve-static |
| setprototypeof | 1.2.0 | ISC | https://github.com/wesleytodd/setprototypeof |
| side-channel-list | 1.0.1 | MIT | https://github.com/ljharb/side-channel-list |
| side-channel-map | 1.0.1 | MIT | https://github.com/ljharb/side-channel-map |
| side-channel-weakmap | 1.0.2 | MIT | https://github.com/ljharb/side-channel-weakmap |
| side-channel | 1.1.1 | MIT | https://github.com/ljharb/side-channel |
| socket.io-adapter | 2.5.7 | MIT | https://github.com/socketio/socket.io |
| socket.io-parser | 4.2.7 | MIT | https://github.com/socketio/socket.io |
| socket.io | 4.8.4 | MIT | https://github.com/socketio/socket.io |
| stack-trace | 0.0.10 | MIT | https://github.com/felixge/node-stack-trace |
| statuses | 2.0.2 | MIT | https://github.com/jshttp/statuses |
| string_decoder | 1.3.0 | MIT | https://github.com/nodejs/string_decoder |
| supports-color | 11.0.0 | MIT | https://github.com/chalk/supports-color |
| tar | 7.5.22 | BlueOak-1.0.0 | https://github.com/isaacs/node-tar |
| text-hex | 1.0.0 | MIT | https://github.com/3rd-Eden/text-hex |
| toidentifier | 1.0.1 | MIT | https://github.com/component/toidentifier |
| triple-beam | 1.4.1 | MIT | https://github.com/winstonjs/triple-beam |
| tweetnacl | 1.0.3 | Unlicense | https://github.com/dchest/tweetnacl-js |
| type-is | 2.1.0 | MIT | https://github.com/jshttp/type-is |
| undici-types | 8.9.0 | MIT | https://github.com/nodejs/undici |
| unpipe | 1.0.0 | MIT | https://github.com/stream-utils/unpipe |
| util-deprecate | 1.0.2 | MIT | https://github.com/TooTallNate/util-deprecate |
| vary | 1.1.2 | MIT | https://github.com/jshttp/vary |
| web-streams-polyfill | 3.3.3 | MIT | https://github.com/MattiasBuelens/web-streams-polyfill |
| winston-transport | 4.9.0 | MIT | https://github.com/winstonjs/winston-transport |
| winston | 3.19.0 | MIT | https://github.com/winstonjs/winston |
| wrappy | 1.0.2 | ISC | https://github.com/npm/wrappy |
| ws | 8.21.3 | MIT | https://github.com/websockets/ws |
| yallist | 5.0.0 | BlueOak-1.0.0 | https://github.com/isaacs/yallist |

---

## Admin Portal Production Dependencies

The following npm packages are included in the admin portal production bundle
(client/admin). The `admin-ui-builder` stage compiles them and the resulting
`dist` is copied into the control-plane runtime image at `/admin-ui`
(`services/control-plane/Dockerfile:8,66`), so they are distributed with that
image even though the image's Go table above lists only linked Go modules.

Versions here are resolved from `client/admin/package-lock.json` and are
independent of the media-plane lockfile: the two are bumped by separate
Dependabot PRs and may drift apart at any time. Do not read either table as
covering the other.

| Package | Version | License | Repository |
|---|---|---|---|
| @types/react | 19.3.0 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| csstype | 3.2.3 | MIT | https://github.com/frenic/csstype |
| lucide-react | 1.61.0 | ISC | https://github.com/lucide-icons/lucide |
| react-dom | 19.3.1 | MIT | https://github.com/react/react |
| react | 19.3.1 | MIT | https://github.com/react/react |
| scheduler | 0.28.0 | MIT | https://github.com/react/react |

---

## Build-Time and Development Dependencies

Additional npm and Go packages are used during development, build, test,
and tooling but are **not** included in distributed binaries. These
include build tools, test frameworks, linters, formatters, type checkers,
and similar development utilities. A complete listing is available in the
respective `package.json` (dev dependencies) and `go.mod` (transitively
through dev tools like `golangci-lint`) at the time of build. Notable
build-time-only attributions:

- **lightningcss** (MPL-2.0) — CSS transformer used by Vite/Parcel at
  build time; not modified by Concord Voice; not included in runtime
  bundles.
- **caniuse-lite** (CC-BY-4.0) — browser compatibility data used by build
  tools; attribution acknowledged.
- **spdx-exceptions** (CC-BY-3.0) — SPDX standard license exceptions
  metadata; attribution acknowledged.
- **HashiCorp libraries** (`errwrap`, `go-cleanhttp`, `go-immutable-radix`,
  `go-multierror`, `go-retryablehttp`, `go-version`, `golang-lru`, `hcl`,
  all MPL-2.0) — used transitively by Go dev tooling; not linked into
  control-plane runtime binary.
- **avast/apkparser** (LGPL-3.0) — Android APK parser used transitively
  by a dev tool; not linked into any shipped binary (confirmed via
  `go mod why github.com/avast/apkparser` returning "main module does
  not need package").
- **"cure for AI slop" kit** (MIT, Copyright (c) 2026 Ege Çelebi) —
  **vendored source, not a managed package.** The ASD-STE100 writing skill
  and its `ste-lint.py` heuristic linter, copied into
  `[internal]skills/ste-writing/` and its generated mirrors under
  `.github/skills/`, `.gemini/skills/`, and `.codex/skills/`. Repo tooling
  only; not linked into or bundled with any shipped artifact. Modified by
  Concord Voice — local changes are listed in the linter's file header. The
  full MIT copyright and permission notice is preserved at
  `[internal]skills/ste-writing/LICENSE.upstream`. Upstream:
  <https://github.com/woosal1337/blog/tree/b912d5fa59f368253683af2ebfac64ad6d08312d/videos/ep01-the-cure-for-ai-slop>.
  Because it is vendored rather than resolved by a package manager, it will
  **not** appear in regenerated dependency listings — update this entry by
  hand if the vendored copy changes.

## Regenerating This File

This NOTICE file should be regenerated whenever dependencies are added,
removed, or upgraded. Tooling:

**Prefer the script.** [`scripts/update-notice-md.py`](scripts/update-notice-md.py)
runs everything below and rewrites the three tables and the summary counts in one
step. It is dry-run by default; `--apply` writes.

```bash
(cd .github/tools/license-checker && npm ci --ignore-scripts --no-audit --no-fund) && \
(cd services/media-plane && npm ci) && (cd client/admin && npm ci) && ./scripts/update-notice-md.py --apply
```

It exits **2** rather than writing when a Go module is linked into **any of the
three shipped binaries** with no licence recorded here: `go list` reports no
licence, and inventing one would assert a licence determination, which is an
audit action. Determine it from the module's own `LICENSE` file and add the row
by hand. It also exits 2 for a licence outside the compatibility allowlist, and
for any field containing a `|` that would forge a table column.

`.github/workflows/license-check.yml` runs the same script with `--check`, which
FAILS on an attribution defect — a shipped component with no row, a row for
something no longer shipped, a changed licence (**npm only**, see below), or a
stated count that disagrees with its own table — and merely warns on version
drift, which [`/weekly-deps`]([internal]skills/weekly-deps/SKILL.md) Phase 6.3
clears weekly. A red guard is visible but does **not** block merge: it is not a
required status check (the org baseline ruleset requires `Gitar` and
`SonarCloud Code Analysis`, and the Alpha-only ruleset adds
`Required PR build result`).

CI checks npm workspaces independently. A workspace manifest or lockfile change
checks that workspace only; NOTICE, workflow, selector-policy, or regenerator
changes check both. A Go source change also runs the Go license check,
because imports can change the runtime dependency set without changing a module
file. Scoped npm checks use `--check --scope npm --workspace <workspace>` and
still validate the complete NOTICE summary and table counts.

A Go upstream **relicence is structurally invisible** to the script. `go list`
reports no licence, so each module's licence is carried forward from the table
above rather than re-read; only npm licences are re-derived every run. The
compensating control is manual and lives in `/weekly-deps` Phase 6.3 — diff the
module cache's `LICENSE` across the two versions whenever a Go pin moves.

The commands the script runs, for reference and for regenerating one family by
hand, from the repository root:

`--excludePrivatePackages` is what drops the first-party rows: Concord Voice's
own packages and the local `node-domexception` stub override (documented in
`[internal]rules/media-plane.md` "Docker Build Context Invariant") all declare
`"private": true`. Do **not** substitute `--excludePackages` with a
semicolon-separated name list — that flag matches on the full `name@version`
key, so a bare name never matches and the exclusion silently does nothing.
`.github/workflows/license-check.yml` uses the same flag, so the two paths agree.

```bash
(cd .github/tools/license-checker && npm ci --ignore-scripts --no-audit --no-fund)
notice_checker="$PWD/.github/tools/license-checker/node_modules/.bin/license-checker"

# Media-plane (npm)
(cd services/media-plane && "$notice_checker" --production --csv --excludePrivatePackages > /tmp/media-licenses.csv)

# Admin portal (npm) -- bundled into the control-plane runtime image at /admin-ui
(cd client/admin && "$notice_checker" --production --csv --excludePrivatePackages > /tmp/admin-licenses.csv)

# Go control-plane (binary runtime modules). All THREE shipped binaries, not
# just the server -- a module reachable from any of them is distributed. Lines
# with an empty version are first-party module paths; drop them.
(cd services/control-plane && go list -deps -e -f '{{if .Module}}{{.Module.Path}} {{.Module.Version}}{{end}}' ./cmd/server ./cmd/migrate-media ./cmd/mfa-rekey | sort -u)
```

Each table below is sorted the way `license-checker` sorts its CSV: by the full
`name@version` key, which is why `@types/estree-jsx` precedes `@types/estree`
(`-` sorts before `@`). Preserve that order so regenerated output diffs cleanly.

See [docs/legal/dependency-license-audit.md](docs/legal/dependency-license-audit.md)
for the full audit methodology and license-compatibility framework.

## Contact

For licensing questions about Concord Voice's use of these dependencies,
contact: [contact-us@concordvoice.com](mailto:contact-us@concordvoice.com).

For licensing questions about Concord Voice itself, see [LICENSE](./LICENSE)
and [docs/legal/commercial-license.md](docs/legal/commercial-license.md).
