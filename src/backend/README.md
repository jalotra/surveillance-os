# surveillance-os — hub

One Go process on a disposable DigitalOcean droplet. It ingests fMP4
fragments from Raspberry Pi cameras over the tailnet, fans live video out
to browsers, writes 10-minute chunks + manifests to Cloudflare R2, and
keeps the catalog in Postgres. All local state is disposable — Postgres
and R2 are the truth. Reference: `docs/architecture.md`, `plans/backend.md`.

## Layout

```
cmd/hub/              server binary — wires everything
cmd/rebuild-catalog/  disaster recovery: rebuild Postgres from R2 objects
internal/proto/       wire format: [u32 header_len][header JSON][payload]
internal/ingest/      /ingest WebSocket (Tailscale whois auth), seq/pts
                      validation, acks, suspect-clock + gap events
internal/ring/        per-cam 5-minute hot buffer; COW subscribers
internal/live/        /live/{cam} viewer WebSocket (init + keyframe-forward)
internal/chunker/     10-min windows → scratch → R2 mp4+manifest → Postgres
internal/store/       pgx catalog (cameras/chunks/events/gaps)
internal/blob/        R2 (S3): PutObject, presigned GetObject, ListObjectsV2
internal/api/         REST: /api/cameras, /api/chunks*, /api/events,
                      /api/thumb/{cam}, /status, /healthz
internal/alerts/      30s loop: silence >2min or throttled!=0 → Telegram
internal/config/      env parsing
migrations/001_init.sql   catalog schema
```

## Endpoints

| route | purpose |
|---|---|
| `WS /ingest` | Pi agent → hub. Tailnet-only, **never** behind Cloudflare Tunnel |
| `WS /live/{cam}` | browser live view (behind Cloudflare Access) |
| `GET /api/cameras` | camera rows + last_seen/status |
| `GET /api/chunks?cam=&from=&to=` | chunk metadata |
| `GET /api/chunks/{cam}/{window}/urls` | presigned mp4+manifest URLs (5 min TTL) |
| `GET /api/events?cam=&from=&to=` | gap/silence/suspect_clock events |
| `GET /api/thumb/{cam}` | latest JPEG thumbnail |
| `GET /status`, `/healthz` | ops |

## Configuration (env)

| var | default | notes |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | bind the tailnet IP in prod |
| `INGEST_AUTH` | `tailscale` | `none` = dev bypass (no whois) |
| `TAILSCALE_SOCK` | `/run/tailscale/tailscaled.sock` | LocalAPI |
| `CAMERAS` | — | `id:site:tailnet_name;...` |
| `DATABASE_URL` | — | unset → `store.Null` dev mode |
| `R2_ENDPOINT`, `R2_BUCKET`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY` | — | unset → `blob.Discard` dev mode |
| `R2_PREFIX` | `surveillance` | object key prefix |
| `CHUNK_SCRATCH_DIR` | `/var/lib/surveillance-hub/chunks` | open-window scratch |
| `RING_MAX_AGE`, `RING_MAX_BYTES` | `5m`, `64MiB` | hot buffer limits |
| `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID` | — | unset → alerts logged only |

## Dev

```bash
go build ./... && go vet ./... && go test -race ./...

# Run with no external deps (Null store, Discard blob, no whois):
INGEST_AUTH=none CHUNK_SCRATCH_DIR=/tmp/hub-scratch go run ./cmd/hub
```

## Deploy notes

Schema: `psql "$DATABASE_URL" -f migrations/001_init.sql`. The droplet
needs only Tailscale + this binary + a scratch dir; Cloudflare Tunnel
reaches `LISTEN_ADDR` for `/live` and `/api` only — restrict `/ingest`
to the tailnet interface or an Access rule.
