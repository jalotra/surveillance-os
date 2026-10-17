# frontend — the client app (Next.js, behind Cloudflare Access)

Live view via WS + MSE, 14-day timeline scrub via presigned R2 GETs, 3-camera grid.
Reference: `docs/architecture.md` §7.

Keeps `src/frontend/` (Next.js 16, React 19, shadcn/ui) — the shell and components are fine.
What dies: the base64-JPEG-into-`<img>` player and every `localhost` hardcode.

---

## 1. Serving model (decides all URLs)

One Cloudflare hostname fronts both services; Access (Google SSO, email allowlist) gates the
whole domain:

```
surv.example.com/*          → next:3000   (this app)
surv.example.com/api/*      → hub:8080    (REST)
surv.example.com/live/*     → hub:8080    (viewer WebSocket)
```

All API calls are **same-origin, no auth code** — the Access session cookie covers `/api` and
`/live` automatically. `NEXT_PUBLIC_*` nothing; a single `lib/config.ts` reads `window.location`
for the WS base (`wss://${location.host}/live`).

(If you take the tailnet-only option instead: same code, just a `http://hub.tailnet:8080` base
and plain `ws://` — keep it behind one config function either way.)

---

## 2. Interfaces

### 2.1 Consumes (from `plans/backend.md`)

| call | used by |
|---|---|
| `GET /api/cameras` | grid page |
| `GET /api/chunks?cam&from&to` | timeline |
| `GET /api/chunks/{cam}/{w}/urls` → `{manifest_url, mp4_url}` | timeline playback |
| `GET /api/events?cam&from&to` | timeline overlay (gap ticks) |
| `GET /api/thumb/{cam}` | grid cards (5 s polling) |
| `GET /status` | status dashboard |
| `WS /live/{cam}` | live view — `init` msg, then `frag`s, `gap` notices |

Envelope decode is shared with the Pi wire format: `[u32 header_len][header JSON][payload]`.

### 2.2 Produces (internal)

```ts
// lib/mse-player.ts — the only non-trivial module
class MsePlayer {
  constructor(video: HTMLVideoElement, mime: string)   // 'video/mp4; codecs="avc1.640028"'
  pushInit(bytes: Uint8Array): void                    // ftyp+moov, once
  pushFragment(bytes: Uint8Array, ptsMs: number): void // moof+mdat
  destroy(): void
}
```

Behavior, all inside the player:

- append queue draining on `sourceopen`/`updateend`; **never call appendBuffer while `updating`**
- target buffer ~1 s: if `buffered.end - currentTime > 2`, `playbackRate = 1.05` (catch up);
  `> 4`, seek to `buffered.end - 0.5` (skip); back to 1.0 under 1 s
- `waiting`/`stalled` for >2 s on live → caller re-subscribes (server resends from keyframe)
- MSE support check: `MediaSource.isTypeSupported(mime)` — on old iOS Safari fall back to
  ManagedMediaSource; if neither, show "live not supported, use history" for that device

```ts
// lib/live-socket.ts — WS → MsePlayer
connect(camId, player): { close(): void }   // auto-reconnect 1→10 s backoff
```

```ts
// lib/history.ts — timeline playback
loadManifest(camId, windowStart): Promise<Manifest>
playFrom(camId, wallClockMs): HistorySession
  // finds covering chunk, fetches manifest, seeks nearest keyframe ≤ t,
  // then Range-GETs (bytes=keyframes[i].off..next_off-1) → player.pushFragment,
  // walks chunk boundaries via /api/chunks
```

Seeking inside a chunk: keyframe byte offsets from the manifest → `Range: bytes=off-` —
no server-side segmenting, no HLS.

---

## 3. Data models (client-side types)

```ts
// Field names below mirror the hub's actual JSON output (internal/api).
interface Camera  { id: string; site: string; status: 'online'|'offline';
                    last_seen: string; temp_c: number; spool_bytes: number;
                    thumb_url: string }
interface Chunk   { window_start: number; window_end: number; bytes: number;   // unix seconds
                    duration_s: number; dropped_s: number; suspect: boolean }
// Manifest mirrors the sidecar the chunker writes to R2 ({cam}/{idx}.json):
// idx is the window index (window_start = idx*600s), keyframes carry byte
// offsets for Range-GET seeking.
interface Manifest{ cam: string; idx: number; codec: string; codec_str: string;
                    w: number; h: number;
                    keyframes: { t_ms: number; off: number }[];
                    seqs: string[]; last_seq: number; last_end_ms: number;
                    dropped_s: number; suspect: boolean; finalized: boolean }
interface HubEvent{ id: number; cam_id: string; ts: string;
                    kind: 'gap'|'silence'|'suspect_clock'|'offline'|'online';
                    meta: Record<string, unknown> }
interface HubStatus{ uptime_s: number; goroutines: number;
                    mem_alloc_mb: number; mem_sys_mb: number; num_cpu: number;
                    go_version: string; time: string;
                    cameras: {id: string; online: boolean; last_frag_age_s: number;
                    ring_frags: number; ring_bytes: number; ring_subs: number;
                    suspect: boolean}[] }
```

---

## 4. Code model

```
src/frontend/
  app/
    page.tsx                    -- grid: 3 CameraCards (thumb + status + last_seen)
    cam/[id]/page.tsx           -- live view + inline mini-timeline link
    cam/[id]/history/page.tsx   -- 14-day scrubber + playback
  lib/
    config.ts                   -- apiBase()/wsBase() from location (~15 lines)
    mse-player.ts               -- the MSE wrapper (~150)
    live-socket.ts              -- live WS + reconnect (~80)
    history.ts                  -- manifest/range/timeline logic (~120)
    api.ts                      -- typed fetches (~60)
    types.ts                    -- the interfaces above
  components/
    video-player.tsx            -- REWRITE: <video> + MsePlayer + live-socket
    camera-card.tsx             -- grid card: <img src=/api/thumb/{cam}> + status badge
    timeline.tsx                -- scrub bar: chunks rendered as blocks, gaps as red ticks
    status-dashboard.tsx        -- retool to hub /status shape (drop Pi-specific fields)
```

Deletes: `components/example.tsx`, `components/component-example.tsx`, the base64 `<img>` path
in `video-player.tsx`, every `localhost:8080/8081` literal.

### Page behavior notes

- **Grid**: thumbs via plain `<img>` polling `/api/thumb/{cam}` every 5 s — no MSE pipelines on
  the grid (3 MSE instances on a phone is a bad time).
- **Live page**: one WS → MsePlayer. Show "reconnecting / waiting for keyframe" states — a fresh
  viewer can wait up to one GOP (2 s) by design.
- **History**: timeline = 24 h strip of chunk blocks + event ticks; click → `playFrom(t)`. Past
  `suspect` chunks render amber, gaps render red — surface them, don't hide them.
- Keep shadcn Cards/Badges for chrome; the player itself is a bare `<video>`.

---

## 5. Build order

1. `config.ts` + `mse-player.ts` + `live-socket.ts` + new `video-player.tsx` — live view works
   on `/cam/[id]` (weekend-2 goal: phone on 4G through Access).
2. Grid page with thumbs + `/api/cameras` status.
3. History page: `/api/chunks` → timeline → manifest + Range playback. This is the risky part —
   the `Range`/`MediaSource` dance wants a real chunk to test against; stub with a static fMP4
   file first if the hub isn't ready.
4. `status-dashboard` retool + events overlay on the timeline.

## 6. Out of scope

Auth (Cloudflare Access), multi-user anything, notifications UI, bandwidth adaptation
(substream switching), audio — same "later" list as the backend.
