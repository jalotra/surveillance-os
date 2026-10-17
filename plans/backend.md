# backend — the hub (DigitalOcean droplet, BLR1)

One Go process that ingests fragments from 3 Pis over the tailnet, fans live video out to
browsers, writes 10-minute fMP4 chunks + manifests to R2, and keeps the catalog in PlanetScale
Postgres. Reference: `docs/architecture.md` §2, §6.

Repurposes `src/backend/` — the go4vl capture and Kafka streamer get deleted (capture moves to
the Pis, Kafka is gone). Keep `internal/api` as a starting skeleton only.

---

## 1. Interfaces

### 1.1 Ingest WebSocket — `ws://hub:8080/ingest` (tailnet only)

One long-lived WS per camera. Bound to the tailnet interface only — never exposed through
Cloudflare Tunnel. Auth = tailscaled LocalAPI `whois` on the remote IP; the resolved node name
must match `cameras.tailnet_name` for the `cam` claimed in `hello`.

**Wire envelope** — every message is one binary WS frame:

```
[u32 header_len][header JSON][payload bytes]
```

| type | dir | header fields | payload |
|---|---|---|---|
| `hello` | pi→hub | `cam`, `boot_id`, `fw`, `codec` (`"h264"`), `codec_str` (`"avc1.640028"`), `w`, `h`, `fps`, `first_seq` | — |
| `hello_ok` | hub→pi | `resume_from_seq` (hub's last acked seq for this boot) | — |
| `frag` | pi→hub | `seq`, `pts_ms`, `dur_ms`, `key` (0/1), `replay` (0/1) | moof+mdat bytes |
| `init` | pi→hub | — | ftyp+moov bytes |
| `thumb` | pi→hub | `pts_ms` | JPEG bytes |
| `stat` | pi→hub | `uptime_s`, `temp_c`, `throttled`, `spool_bytes`, `dropped_s_total`, `wifi_rssi` | — |
| `ack` | hub→pi | `last_seq` | — |
| `cfg` | hub→pi | `bitrate`, `fps`, `live` (0/1; null/absent = unchanged) | — |

Rules:

- `ack` every 20 frags or 5 s, whichever first. The Pi uses it as the spool trim point.
- `pts_ms` must be monotonic within a `boot_id`. Violation → close WS, log `suspect_clock` event.
- Clock skew vs hub time never drops data — window gets `suspect=true` in the catalog instead.
- Fragments with `replay=1` may carry `pts_ms` in already-finalized windows — route to the
  chunker anyway (it reopens windows, §3.3).

### 1.2 Live WebSocket — `/live/{cam}` (behind Cloudflare Tunnel + Access)

Viewer-facing. Same envelope format. Server sends:

1. `init` message (ftyp+moov)
2. the **most recent keyframe fragment** forward, then live `frag` messages as they arrive
3. `gap` control message when the camera reports dropped seconds (UI shows a tick)

Backpressure: each subscriber has a bounded chan (cap 4). Full → drop fragments for that
subscriber and mark it `needs_key`; on next keyframe, resync it. Never let one slow socket
stall the camera.

### 1.3 REST (behind Cloudflare Tunnel + Access)

| endpoint | returns |
|---|---|
| `GET /api/cameras` | `[{id, site, status, last_seen, temp_c, spool_bytes, thumb_url}]` |
| `GET /api/chunks?cam=&from=&to=` | `[{window_start, window_end, bytes, duration_s, dropped_s, suspect}]` |
| `GET /api/chunks/{cam}/{window}/urls` | `{manifest_url, mp4_url}` — presigned R2 GETs, 5 min TTL |
| `GET /api/events?cam=&from=&to=` | gap / silence / suspect_clock / online-offline events |
| `GET /api/thumb/{cam}` | latest JPEG snapshot (in-RAM, written by `thumb` messages) |
| `GET /status` | hub health: uptime, per-cam last-fragment age, ring depth, goroutines, mem |
| `GET /healthz` | `OK` for the compose healthcheck |

### 1.4 External dependencies

- **Postgres (PlanetScale)** — `pgx` pool, 4 tables below. All writes retried; never on the
  ingest hot path.
- **R2** — `aws-sdk-go-v2/service/s3` with R2 endpoint + creds from env. `PutObject` for chunks,
  `GetObject` presign for playback, `ListObjectsV2` for `rebuild-catalog`.
- **tailscaled** — `tailscale.com/client/tailscale` `WhoIs` for ingest auth.
- **Telegram Bot API** — single `sendMessage` POST for alerts.

---

## 2. Data models

### 2.1 Postgres

```sql
create table cameras (
  id           text primary key,          -- 'home-a', human-set slug
  site         text not null,
  tailnet_name text not null,             -- expected whois node name
  status       text not null default 'offline',
  last_seen    timestamptz,
  fw_version   text,
  created_at   timestamptz not null default now()
);

create table chunks (
  cam_id       text not null references cameras(id),
  window_start timestamptz not null,
  window_end   timestamptz not null,
  r2_key       text not null,
  manifest_key text not null,
  bytes        bigint not null,
  duration_s   real not null,
  sha256       text not null,
  dropped_s    real not null default 0,
  suspect      boolean not null default false,
  created_at   timestamptz not null default now(),
  primary key (cam_id, window_start)      -- makes the chunker idempotent
);

create table events (
  id     bigserial primary key,
  cam_id text not null references cameras(id),
  ts     timestamptz not null default now(),
  kind   text not null,                   -- gap|silence|suspect_clock|offline|online
  meta   jsonb
);

create table gaps (
  cam_id   text not null references cameras(id),
  ts_start timestamptz not null,
  ts_end   timestamptz,
  reason   text,                          -- spool_overflow|offline|suspect_clock
  primary key (cam_id, ts_start)
);
```

### 2.2 R2 objects

```
surveillance/{cam}/{window_start_unix}.mp4    -- init + fragments, one fMP4
surveillance/{cam}/{window_start_unix}.json   -- manifest
```

```json
{ "cam": "home-a", "window_start": 1737000000, "duration_s": 600, "bytes": 52428800,
  "codec": "h264", "codec_str": "avc1.640028", "width": 1920, "height": 1080,
  "keyframes": [{"t_ms": 0, "off": 0}, {"t_ms": 2000, "off": 174233}],
  "sha256": "...", "dropped_s": 0, "suspect": false }
```

### 2.3 In-memory

```go
type Fragment struct {           // shared shape with the Pi agent (duplicated, ~30 lines, deliberate)
    Seq   uint64
    PtsMs int64
    DurMs uint32
    Key   bool
    Replay bool
    Data  []byte                  // moof+mdat
}

type Ring struct {               // per camera, one owner goroutine
    Init  []byte                  // latest ftyp+moov
    Frags []Fragment              // last 5 min (~26 MB at 0.7 Mbps)
    LastKeyIdx int                // index of newest keyframe
    Subs  atomic.Pointer[[]chan OutMsg]   // copy-on-write, no locks on hot path
}

type Window struct {             // chunker's open window
    CamID, Start int64
    File  *os.File                // hub-disk scratch, ~52 MB max
    FirstKeyOff int64
    Keyframes []KeyOff            // {t_ms, off} for the manifest
    Hasher hash.Hash
}
```

---

## 3. Code model

```
src/backend/
  cmd/hub/main.go                  -- wire everything, config from env
  cmd/rebuild-catalog/main.go      -- LIST bucket → fetch manifests → upsert Postgres
  internal/
    proto/    envelope encode/decode, message types          (~150 lines)
    ingest/   WS upgrade, whois auth, hello/frag validation  (~300)
    ring/     Ring, subscribe/unsubscribe, keyframe join     (~150)
    live/     viewer WS loop, drop-on-slow                   (~150)
    chunker/  window manager, file assembly, R2 put, upsert  (~350)
    store/    pgx queries                                    (~150)
    blob/     R2 client: put, presign, list                  (~100)
    api/      REST handlers (keep current server.go style)   (~250)
    alerts/   silence check every 30 s → telegram            (~100)
    config/   env struct                                     (~50)
```

### Goroutine model

- one `cameraOwner` goroutine per camera owns: ingest WS read → validate → `Ring.Append` →
  `Chunker.Feed`. No shared locks.
- `Ring.Append` writes to subs' chans non-blocking (`select { case ch<-f: default: mark needs_key }`).
- chunker runs inside the owner goroutine — fragments arrive in order, windows are per-camera.
- alert loop + HTTP servers are independent.

### Key signatures

```go
// ingest
func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) // upgrade, whois, hello, loop
func (s *Server) validate(f Fragment, st *CamState) error         // monotonicity, seq continuity

// ring
func (r *Ring) Append(f Fragment)
func (r *Ring) Subscribe() (init []byte, fromKey []Fragment, ch <-chan OutMsg)
func (r *Ring) Unsubscribe(ch <-chan OutMsg)

// chunker
func (c *Chunker) Feed(f Fragment)         // route by floor(pts_ms/600_000)
func (c *Chunker) finalize(w *Window)      -- R2 put mp4+json → Postgres upsert (retry queue)
func (c *Chunker) reopen(cam string, pts int64) // late fragment → closed window rewrite
```

### Ordering rule (load-bearing)

R2 `PutObject` (mp4 + json) **completes before** the Postgres upsert, and the upsert retries
with backoff. A PlanetScale outage must never lose footage — worst case is a missing row that
`rebuild-catalog` backfills.

---

## 4. Build order (matches architecture §10, weekend 2)

1. `proto` + `ingest` + `ring` + `live` — laptop can receive a Pi stream and fan it to a browser.
2. `chunker` + `blob` + `store` — chunks land in R2, rows in Postgres. Kill-test: restart hub
   mid-window, verify the window finalizes cleanly from spool replay.
3. `api` + `alerts` — timeline data + the silence ping.
4. `rebuild-catalog` — run it against an empty schema to prove R2 is the real source of truth.

## 5. Out of scope

Auth code (Cloudflare Access owns it), multi-tenancy, WebRTC, S2, motion detection, audio.
