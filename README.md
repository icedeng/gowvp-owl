**English** | [简体中文](./README_CN.md)

# GoWVP Owl · Video Integration and Open Platform

A video surveillance platform built on [gowvp/owl](https://github.com/gowvp/owl), with continued development for GB/T 28181 device integration, live monitoring, recordings, snapshots, and third-party business applications.

This fork uses AI-assisted development for ongoing code analysis, feature development, bug fixes, testing, and documentation. Maintainers lead requirements and key decisions; releases still require code checks and validation with actual devices and business workflows.

This repository maintains its own Vue 3 admin UI, protocol adaptations, media node management, and application integration platform. Use this repository's code and documentation as the reference for features, configuration, and delivery. The upstream demo, frontend archive, and public images do not represent this fork. See [VERSION](./VERSION) for the release version and [release records](./deploy/releases/) for changes and deployment evidence.

## Features

| Module | Implemented capabilities |
| --- | --- |
| Live monitoring | Overview dashboard, resource tree, multiple video panes, channel details, main/substream selection, playback URL copying, and player settings |
| Device integration | GB/T 28181, ONVIF client and virtual device server, RTSP pulling, RTMP publishing, device/channel management, and heartbeat/registration history |
| GB/T 28181 | Registration authentication, version negotiation, catalog and information queries, live streaming, device recording search/playback/download, PTZ, voice broadcast and intercom, platform cascading, and protocol diagnostics |
| Platform recordings | Manual recording per channel, date search, calendar and continuous playback, downloads, retention policies, and disk usage cleanup |
| Snapshots | Device uploads or FFmpeg frame extraction, background tasks, schedules, image preview/download/rename/delete, global history, and capture settings |
| Media nodes | ZLMediaKit / Lalmax node creation, editing, enable/disable/delete, runtime inspection, channel binding, and per-node callback isolation |
| Third-party applications | Application management, independent AK/SK credentials, API scopes, device grants, source restrictions and quotas, live playback, device/platform recordings, snapshots, PTZ, and event subscriptions |
| Operations and security | Account settings, encrypted login compatible with HTTP/HTTPS, login protection, monitoring, playback credential checks, and signed upgrades/rollback with a companion host service |
| AI and events | Optional AI service integration, analysis tasks, event queries, images, and webhook delivery; requires a separately deployed analysis service |

Protocol profiles cover `1.0=2011`, `1.1=the 2011 amendment (2014)`, `2.0=2016`, and `3.0=2022`. Features follow each profile's capabilities. Optional SIP-TLS, certificate authentication, and Annex G / O functionality require additional configuration. Implementation and automated checks do not establish interoperability with every vendor, standard requirement, or network. See the [completion audit](./docs/testing/gb28181-completion-audit.md) and [interoperability runbook](./docs/testing/gb28181-interoperability-runbook.md).

Media outputs include HTTP-FLV, WS-FLV, HLS, WebRTC, RTSP, and RTMP. Availability, codec decoding, and browser compatibility depend on the media driver, source encoding, and player. Multiple media nodes do not provide automatic load balancing or failover. The admin UI is primarily in Chinese.

## Architecture

```text
Cameras / NVRs / downstream platforms -- SIP, RTP/RTCP --+
RTSP / RTMP / ONVIF devices ---------------------------+
                                                      |
Browsers -------- Admin UI / API ---- GoWVP Owl ---- Media nodes
Application backends -- Signed API ------|                  |
Third-party players -- Ticketed URLs ---|                  |
                                        |                  |
                                    PostgreSQL      Shared recordings
```

- Backend: Go, Gin, and GORM. Frontend: Vue 3, TypeScript, Vite, Pinia, and ECharts.
- Frontend source lives in `admin-ui/`. Release assets are included at `/app/www` in the application image; Owl serves `/web/` and the API on the same origin.
- The recommended separate-service deployment uses Owl + ZLMediaKit + PostgreSQL. ZLM uses host networking, Owl connects through a bridge network, and both containers use the same absolute recording path.
- SQLite and MySQL are also supported. Changing the database connection string does not migrate existing data. See the [PostgreSQL guide](./deploy/postgresql.md) for migration tools and data handling.

## Deploying a New Environment

These commands are for a **new Linux Docker environment**. For existing installations, read the [deployment and upgrade guide](./deploy/v1/README.md), back up data, and review version differences and database migrations first. Dedicated servers use their own runtime configuration.

The build host needs Git, Go (see `go.mod`; currently 1.26), Node.js (22 LTS recommended), pnpm (currently pinned to 11.11.0 in `admin-ui/package.json`), OpenSSL, Docker Engine, and Docker Compose. Linux amd64 / arm64 artifacts are supported; cross-platform image builds require platform support. The initial ZLM build also downloads dependencies and compiles native code.

```bash
git clone https://github.com/icedeng/gowvp-owl.git
cd gowvp-owl

# First check out the intended release branch or tag and review VERSION/release records.
# Replace this IP with a host address reachable by devices, upstream platforms, and players.
./deploy/v1/deploy.sh init 192.168.1.100

# Review .env and config.toml as described below before proceeding.
./deploy/v1/deploy.sh release
./deploy/v1/deploy.sh status
```

`init` generates `deploy/v1/.env` with independent random passwords/secrets and refuses to overwrite an existing file. `release` builds the backend, frontend, and local images, then starts the services; it does not push images to a registry. Use `build` to build only, or `up` to start existing images. See the [deployment guide](./deploy/v1/README.md) for all commands.

Review these settings before deployment:

| Location | Required review |
| --- | --- |
| `[Sip]` in `deploy/v1/data/configs/config.toml` | `Host`, 20-digit platform `ID`, 10-digit `Domain`, and device registration policy |
| `deploy/v1/.env` | `OWL_MEDIA_SDP_IP`, HTTP port, administrator credentials, database connection, and independent secrets |
| SIP registration password | `OWL_SIP_PASSWORD` starts empty; set it and configure devices consistently when Digest password validation is required |
| Media nodes | API address reachable by Owl, callback address reachable by the node, device-reachable media address, and shared API secret |
| Storage and networking | Disk capacity, bind mounts, port conflicts, and routes from devices/players to the advertised media address |

After startup, open `http://<host>:15123/web/`; health information is at `/health`. Adjust the port for your deployment. There is no universal login password. Initially use the credentials in `.env`; after changing them in the UI, `admin-credentials.toml` takes precedence over initial environment values and must be backed up.

### Common Ports

These ports follow the `deploy/v1` configuration. Keep device settings, advertised addresses, and firewall rules consistent when changing them.

| Port | Purpose |
| --- | --- |
| `15123/tcp` | Admin UI, API, WebSocket, media HTTP proxy, and ONVIF SOAP |
| `15060/tcp+udp` | GB/T 28181 SIP signaling |
| `15061/tcp` | SIP-TLS; requires certificates and explicit enablement |
| `3702/udp` | ONVIF WS-Discovery; container multicast discovery depends on the network |
| `8080/tcp` | ZLM HTTP/API |
| `554/tcp`, `1935/tcp` | RTSP, RTMP |
| `30000/tcp+udp` | WebRTC |
| `30001–50000/tcp+udp` | Dynamic GB/T 28181 RTP/RTCP ports |
| `50200–50300/tcp` | Optional Annex O raw TCP download/cascade relay |

PostgreSQL uses `5432` inside the Compose network without publishing a host port. The deployment builds local images; upstream `gospace/gowvp:latest` and other upstream images do not include this fork's custom features. See the [server deployment plan](./deploy/server-192.168.6.20.md) for differences between the generic template and a dedicated environment.

## Third-Party Playback and Business Integration

Third-party applications can play platform video without an administrator login session.

| Integration | Use case and authorization boundary |
| --- | --- |
| Shared playback URLs: `/proxy/sms/…` | URLs carry a playback token bound to a media node and stream. External players can use them directly; HLS manifests and child resources retain credentials. Cross-stream/node access and management API forwarding are blocked. Possession grants playback access to that stream, not application-level resource authorization |
| Application APIs: `/openapi/v1/` + `/openmedia/v1/` | An application backend signs requests with AK/SK and obtains short-lived playback tickets subject to scopes, device grants, source restrictions, and quotas. Use this for independent application permissions, revocation, auditing, and callbacks |

The integration platform provides device/channel lookup by code, live sessions, device recording catalogs and playback, platform recordings, snapshot tasks, PTZ, events, and subscriptions. Keep AK/SK credentials on the application backend; give players media URLs only.

The integration platform is disabled by default. Run its dedicated migration against the existing database, configure an independent keyring, Audience, and Public URL, then enable it. It currently supports a single application instance; adding replicas cannot safely share its workers. Application-level isolation also requires controlling network access to legacy APIs, direct media ports, and internal webhooks as documented.

- [Deployment, application authorization, and business APIs](./docs/open-platform/README.md)
- [Signing protocol](./docs/open-platform/signing-v1.md) · [OpenAPI contract](./docs/open-platform/openapi.yaml)
- Public developer documentation on your instance: `/web/#/open-platform/docs`. Reading requires no administrator login; API authentication still applies.
- Management API documentation on your instance: `/swagger/index.html`. Use the current code and generated documentation as the reference.

Ordinary management routes can use `[Server.HTTP].AuthURL` for external authentication. This is separate from AK/SK integration; application administration and version upgrades still require an administrator JWT.

## Recordings, Snapshots, and Operations

- **Recordings:** The generic deployment allows manual recording without automatically recording every channel. Recording continues until explicitly stopped, even after closing the page. Without recording or another keepalive consumer, streams stop after the configured no-viewer delay, which defaults to 180 seconds.
- **Cleanup:** Recording retention and disk usage checks apply to the filesystem containing recordings. Other applications on a shared disk may trigger cleanup; this is not a hard per-directory quota. Cleanup stops when a failed batch makes no progress and retries in a later cycle.
- **Snapshots:** Successful images and schedules persist. Snapshot history does not expire under recording retention rules; manage capacity and back up the database together with `cover/captures`. See the [snapshot guide](./docs/channel-captures.md).
- **AI:** `deploy/v1` does not include an AI service and explicitly disables it in the template. Deploy and authenticate an analysis service separately before enabling analysis.
- **Upgrades:** `VERSION` defines the release version. Web-based signed upgrades require a host agent, signing keys, and companion configuration, and support compatible releases without database migrations. See the [signed upgrade guide](./deploy/192.168.6.20/UPGRADE.md).
- **HTTP:** Encrypted login and clipboard copying support HTTP admin pages. RSA login does not replace HTTPS protection for sessions and business data. The integration platform requires HTTPS by default; HTTP must be explicitly enabled.
- **Backups:** Retain consistent database backups, configuration, administrator credentials, the application keyring, and required media files. Images alone cannot restore business data. Keep real `.env` files, keys, and databases out of the source repository.

## Development and Repository Layout

The frontend no longer requires an upstream `www.zip` download. Connect the local development server to an existing backend:

```bash
pnpm --dir admin-ui install --frozen-lockfile

# Replace with your backend. The development proxy strips the /api prefix.
VITE_DEV_API_TARGET=http://127.0.0.1:15123 pnpm --dir admin-ui dev
```

Open the local URL printed by Vite. To develop the backend separately, run `go run ./main.go -conf ./configs` after injecting credentials/secrets into the **process environment** using the [.env example](./.env.example) and configuring a working media service and database. `go run` does not automatically load `.env`. Build production frontend assets with `VITE_API_BASE_URL=/ pnpm --dir admin-ui build` to keep the development-only `/api` prefix out of release assets.

```text
admin-ui/             Vue admin UI, player integration, public developer documentation
internal/             Application wiring, configuration, business logic, and APIs
pkg/gbs/              GB/T 28181 SIP, SDP, XML, transactions, and cascading
pkg/zlm/, pkg/lalmax/  Media service adapters
plugin/               Monitoring and common management endpoints
tools/                Migration, documentation generation, signing, and upgrade tools
deploy/               Deployment templates, server configuration, and release records
docs/                 Feature guides, API contracts, protocol audits, and field testing
```

Run checks relevant to the changed code. The frontend `build` includes type checking and integration documentation consistency checks. After changing the integration contract, run `go run ./tools/openplatform-docs` to regenerate resources. Device interoperability, playback decoding, and public-network routing still require environment-specific validation.

## Documentation

Most detailed guides are currently in Chinese.

| Topic | Guide |
| --- | --- |
| Building and first deployment | [Generic deployment](./deploy/v1/README.md) |
| Existing server operations | [Runtime guide](./deploy/192.168.6.20/README.md) · [Deployment plan](./deploy/server-192.168.6.20.md) |
| Databases and migration | [PostgreSQL and device history](./deploy/postgresql.md) |
| Media nodes | [Management and boundaries](./docs/media-node-management.md) · [API](./docs/api/media-nodes.md) |
| Snapshots and schedules | [Channel snapshots](./docs/channel-captures.md) |
| Third-party integration | [Application platform](./docs/open-platform/README.md) · [PTZ API](./docs/api/ptz.md) |
| GB/T 28181 compatibility and validation | [Completion audit](./docs/testing/gb28181-completion-audit.md) · [Device matrix](./docs/testing/gb28181-device-matrix.md) · [Release checks](./docs/testing/gb28181-release-validation.md) |
| Protocol extensions | [Annex G implementation](./docs/GB28181-annex-g-implementation.md) · [Annex O design](./docs/adr/gb28181-2014-direct-tcp-download.md) |
| Upstream adoption | [Assessment and implementation record](./docs/upstream-adoption-review-2026-11-10.md) |
| Public-network ingress planning | [WSS gateway proposal](./docs/国标公网接入网关-WSS方案.md); planned/in-progress work, with delivery scope confirmed by subsequent release records |

## Upstream Credits and License

Based on [gowvp/owl](https://github.com/gowvp/owl), retaining the original project's attribution and copyright notices. Thanks to [ixugo](https://github.com/ixugo), [panjjo/gosip](https://github.com/panjjo/gosip), [goddd](https://github.com/ixugo/goddd), [ZLMediaKit](https://github.com/ZLMediaKit/ZLMediaKit), [Lalmax](https://github.com/q191201771/lalmax), and other open-source contributors.

Upstream fixes are reviewed and adapted individually to the current implementation, with decisions recorded in repository documentation. This repository is licensed under [GNU GPL v3.0](./LICENSE). Modifications and distribution must comply with the license and retain copyright notices. Players, media servers, and other third-party components remain subject to their own licenses and authorization terms.
