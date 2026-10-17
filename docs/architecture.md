# Surveillance OS — 3-camera home setup

Scope: **3 Raspberry Pis at 3 locations, one hub, one user.** Rewritten from the earlier fleet
plan; that one assumed a business with 50+ cameras and was wrong for this.

Target: **~$11/month all-in, ~1 s glass-to-glass, 14 days of footage, no public ports.**

Stack: Pi 4 + Camera Module 3 per site · DigitalOcean droplet (BLR1) running the Go hub ·
PlanetScale Postgres (existing instance) for the catalog · Cloudflare R2 for chunks ·
Tailscale for the device plane · Cloudflare Tunnel + Access for viewing.

---

## 1. What the smaller scope deletes

This is the most valuable part of the revision. Every line below is work you do not have to do
with 3 devices:

| Fleet plan | Home reality |
|---|---|
| Postgres cluster, multi-tenant, per-org ACLs | **your existing PlanetScale Postgres**, one user, 4 small tables |
| Kafka / JetStream event bus | nothing — a channel and a table |
| pi-gen image + cloud-init + A/B partitions + RAUC | flash 3 cards by hand, install with one script |
| Signed apt repo + staged rollout + rollback | `git pull && systemctl restart`, or `scp` a binary |
| mTLS / per-device certs / token minting | Tailscale identity is enough for 3 Pis you own |
| Auth.js + Google OIDC + session code | **Cloudflare Access** (Google SSO, zero code) or tailnet-only |
| WebRTC + SFU + TURN | WebSocket + MSE, ~1 s, one code path |
| Hailo AI HAT, motion pipeline | skip — record 24/7, 14 days, search by time |
| Prometheus + Grafana + CloudWatch | one `/status` JSON + a Telegram ping when a camera is silent |
| Terraform, ECS, ASG | one $5–12 VM and a `docker-compose.yml` |
| R2 presigned uploads from the Pi | hub does the chunking, Pi stays dumb |

Two of these also remove whole subsystems: **Cloudflare Access deletes your auth code**, and
**hub-side chunking deletes the Pi's storage credentials**. The Pi becomes: capture → encode →
spool → push. That's it.

---

## 2. Target design

```
 Site A ─ Pi 4 ─┐
 Site B ─ Pi 4 ─┼── tailnet (WireGuard) ──▶  Hub   (DO droplet, BLR1, Docker Compose)
 Site C ─ Pi 4 ─┘                            ├─ live: 5 min ring in RAM → WS fanout
                                             ├─ chunker: 10 min fMP4 → R2
                                             ├─ catalog: Postgres (PlanetScale)
                                             └─ /status, alerts
                                                    │
                          Cloudflare Tunnel ────────┘
                          + Access (Google SSO)
                                    │
                          Browser: live + timeline scrub
                                    │
                              presigned R2 GET (Range)
```

Why this shape:

> **Load-bearing assumption:** the 3 Pis are in 3 *different* networks, so something has to bridge them.
> If they're actually 3 rooms in one house, delete the VM, the tailnet and Cloudflare, and run the hub on
> a local box — see question 1 at the end.

- **The Pi pushes, never serves.** No public IP, no port forwarding, no CGNAT problem. If the tailnet
  is down it spools to disk and replays with original timestamps.
- **The hub is disposable.** All state lives in PlanetScale + R2 — kill the VM, redeploy, resume.
- **Only credentials on the VM:** R2 + Postgres. Nothing sensitive on the Pis except a tailnet node key.

---

## 3. What we're not using: a stream log (S2)

The original plan routed video through S2 (s2.dev). At this scale it fails on both axes:

| Design | S2 | R2 | VM | **Total/mo** |
|---|---|---|---|---|
| All video through S2 (720p, 24 h retention) | ~$80 | $4 | $6 | **~$90** |
| 360p live substream via S2 + full quality to R2 | ~$34 | $4 | $6 | **~$44** |
| **No S2 — video via tailnet → hub → R2 (chosen)** | **$0** | $4 | $6 | **~$10** |

Two reasons beyond the bill:

- **Latency.** S2 basin locations are limited and there may be no India region — every live
  frame would hairpin internationally on its way to a hub 50 km away.
- **It buys no durability.** The catalog's real source of truth is R2: `LIST` the bucket +
  read the `{window}.json` manifests and every Postgres table can be rebuilt from scratch.
  A mirrored log would just be a dual-write that can silently drift.

Revisit only if the fleet grows past ~10 cameras or live viewing ever needs a durable buffer
between Pi and hub.

---

## 4. Hardware

Buying all three kits new. Prices below.

### Per site

| Item | Pick | Price | Why |
|---|---|---|---|
| Board | **Pi 4 Model B 4 GB** | $55 | **hardware H.264 encoder**, 5 GHz WiFi, 4 GB for spool + future CV |
| Camera | Camera Module 3 (standard 66°) | $25 | wide (102°) for corners, $35 |
| Storage | 64 GB high-endurance microSD | $12 | ring buffer only; `log2ram` + read-only root |
| Power | Official 27 W / 15 W USB-C PSU | $10 | cheap PSU = camera dropouts, the #1 cause of weird bugs |
| Case | official or 3D-printed, with CSI slot | $10 | |
| Ethernet | 1 m Cat5e (optional) | $3 | if a cable is feasible, use it over WiFi |
| **Total** | | **~$115/site → $345** | |

**Do not buy a Pi 5 for this.** It has no hardware video encoder — 1080p30 encode costs 30–40% CPU
([BCM2712 docs](https://github.com/raspberrypi/documentation/blob/90eab3e206563dd53f02b2b41b40ac53f3b6fa5e/documentation/asciidoc/computers/processors/bcm2712.adoc)).
A Pi 4 does the same job at ~5% CPU for less money. If you already own Pi 5s, they work fine at 1080p10
in software with `x264enc tune=zerolatency` — just expect fans.

Cheapest build if budget matters: **Zero 2 W + Camera Module 3 ≈ $70/site** (it also has the hardware
encoder, 1080p30). Two caveats: 2.4 GHz-only WiFi is unreliable for continuous UDP-ish streaming, and
512 MB RAM means a small spool. For three fixed locations I'd pay the extra $45 for the Pi 4.

### Optional

- **Night vision:** Camera Module 3 **NoIR** ($25) + a 850 nm IR illuminator ($10). Don't put it behind
  glass — IR reflects off the window.
- **Audio:** USB mic ($20) muxed as Opus. Home-legal if it's your own home; if the camera is at a
  relative's, get consent and keep it off by default. Ship it in phase 3, not phase 1.
- **UPS:** not needed. Cameras reboot cleanly; make the filesystem read-only so a power cut can't
  corrupt the card.

### Hub VM

- **DigitalOcean droplet in BLR1 (Bangalore).** The $6/mo Basic (1 GB RAM, 1 vCPU, 25 GB SSD) is
  enough; the Go hub with 3 cameras sits around 80–120 MB.
- **Region = closest to the sites.** With no stream-log basin to co-locate with, pick by ping time
  to the Pis. PlanetScale's region only affects timeline-page queries — a few reads/minute,
  tolerable from anywhere.
- One VM is a single point of failure. For a home setup that's an acceptable trade: the Pis spool and
  replay. Don't pay for HA.

---

## 5. Node software

One Go binary per Pi, ~400 lines, systemd unit, `Restart=always`.

```
gstreamer (subprocess) ──fMP4 bytes──▶ Go: splitter ──▶ spool file (1 GB ring)
                                              └──────▶ WS push to hub
```

**Pipeline** (Pi 4, hardware encode, fragmented MP4 out of the muxer so you never transcode):

```bash
gst-launch-1.0 -e libcamerasrc ! video/x-raw,width=1920,height=1080,framerate=10/1 \
  ! v4l2h264enc extra-controls="controls,video_bitrate=700000,video_gop_size=20" \
  ! h264parse config-interval=1 ! mp4mux fragment-duration=1000 ! fdsink fd=1
```

Check the encoder's accepted controls on your board first with
`v4l2-ctl -d /dev/video11 --list-ctrls` (the encoder node is usually `video11` on Pi 4); a bogus
control value makes the pipeline fail at runtime, not at parse time.

Pi 5 substitute: `x264enc tune=zerolatency speed-preset=ultrafast bitrate=700 key-int-max=20`.

**Fragment splitter.** Read from stdout, walk the MP4 box structure (`ftyp`/`moov` once, then repeated
`moof`+`mdat`), emit `{init, fragment}`. You get the init segment for free and each fragment is a
self-contained 1-second unit — exactly the record the hub wants. No re-muxing anywhere in the system.

**Spool.** Append fragments to `spool.bin` with a 24-byte header per fragment
(`magic, boot_id, pts_ms, len`). Keep 1 GB (~3 h at 0.7 Mbps). On reconnect, replay from the last
acked offset, *before* new frames, with their original timestamps. This is the single feature that
makes a home setup feel reliable when the ISP blips.

**Backpressure rule:** capture is never blocked. If the spool is full, drop the oldest fragment and
count the dropped seconds. The hub stores that count as a gap event, and the UI shows a red tick on
the timeline. Never silently pretend the footage exists.

**Don't reimplement camera control.** Do not use `go4vl` on `/dev/video0` for a CSI Camera Module —
that's the USB/UVC path and it won't open the sensor. `libcamerasrc` is the supported path on Pi 4/5.

---

## 6. Hub software

One Go binary; the catalog lives in your existing PlanetScale Postgres (dedicated
database/schema — keep the project self-contained).

**Ingest.** One WebSocket per camera over the tailnet (peer identity via tailscaled `whois` on
the peer IP). Validates `pts_ms` monotonicity *within a `boot_id`*, appends to the camera's ring
and the current chunk buffer. **Never drop on clock skew alone** — a Pi replaying a 3 h spool
legitimately carries old timestamps; if a boot's clock offset exceeds ~5 min, store the footage
but mark its windows `suspect`.

**Live fanout.** Per camera: `init` + last 5 min of fragments in RAM (~26 MB at 0.7 Mbps), subscribers
via a copy-on-write slice. New viewer starts at the **most recent keyframe**, not "now", otherwise the
player stalls for a full GOP. Slow viewer → drop its fragments, don't buffer.

**Chunker.** 10-minute windows on `pts_ms`, aligned to the first keyframe ≥ window start.

- Format: **fragmented MP4**, never plain MP4. Plain MP4 rewrites `moov` at the end; a power cut
  leaves an unplayable file.
- Key: `{cam}/{window_start_unix}.mp4`, upserted — plus `{cam}/{window_start_unix}.json` holding
  the manifest below (keyframe offsets included; scrub = presigned GET on the json, then ranged
  GETs on the mp4). Re-running produces identical bytes → idempotent, so late replay just
  overwrites. A fragment arriving for an already-finalized window reopens it and rewrites both
  objects — that's the other half of why the keys are idempotent.
- Order matters: **R2 objects first, Postgres row second, with retry.** A PlanetScale blip must
  never delay or lose a chunk — the worst outcome is a row a backfill job adds later.
- Do not finalize a window until you've seen data past it, or the camera has been quiet for 60 s.
- Write the manifest with a **byte-offset index per keyframe** — this is what makes scrubbing cheap:

```json
{ "cam": "home-a", "window_start": 1737000000, "duration_s": 600, "bytes": 52428800,
  "codec": "h264", "width": 1920, "height": 1080,
  "keyframes": [{"t_ms": 0, "off": 0}, {"t_ms": 2000, "off": 174233}, ...],
  "sha256": "...", "dropped_s": 0 }
```

Seeking = presigned R2 GET with `Range: bytes=<off>-`, prepend `init`, feed MSE. No transcoding, no
HLS, no server-side segmenting.

**Catalog.** PlanetScale Postgres: `chunks` (PK `cam_id, window_start`), `cameras`, `events`,
`gaps`. Postgres is managed so durability is its job — but the *real* source of truth is R2.
A `rebuild-catalog` command (LIST bucket → fetch manifests → upsert rows) reconstructs every
table, and it's cheap enough to run on a cron as a consistency check.

**Auth: don't write any.** Put Cloudflare Tunnel + **Access** in front of the hub with Google as the
identity provider and an email allowlist. Free for up to 50 users, no open ports, HTTPS, and you get
"sign in with Google" without a line of Auth.js. (Cloudflare Access also covers the R2 presign
endpoint if you keep it on the same hostname.)
Alternative if you only ever view on your own devices: expose the app **only on the tailnet** and skip
auth entirely.

**Ops.** `/status` JSON (uptime, cameras, last fragment age, disk, goroutines). A cron-style check in
the hub that pings you on Telegram if a camera has been silent > 2 min. That's your whole monitoring
stack. On a Pi: `vcgencmd get_throttled` and temperature in the same payload.

---

## 7. Client

Keep the Next.js app you already have; delete the `http://localhost:8080` hardcoding.

- **Live:** `<video>` + `MediaSource`; append `init` then fragments from the WebSocket. ~1 s buffer,
  nudge `playbackRate` to 1.05 when >2 s behind. This is ~150 lines and replaces the base64-over-WS
  `<img>` approach (which wastes 33% on base64 and can't do audio or seeking).
- **Timeline:** a 14-day scrubber backed by `GET /api/chunks?cam=&from=&to=`, then a presigned R2 URL
  per 10-minute block.
- **Grid view:** 3 cameras, MJPEG thumbnail every 5 s is plenty — don't open 3 MSE pipelines on a phone.

Because it's just you: no org model, no roles, no billing. One allowlist of emails.

---

## 8. Storage math (3 cameras, 1080p10 @ 0.7 Mbps)

| | Per camera | 3 cameras |
|---|---|---|
| per day | 7.0 GiB | 21 GiB |
| per 10-min chunk | ~52 MB | — |
| chunks/day | 144 | 432 |
| **14 days retained** | 99 GiB | **295 GiB** |
| R2 cost | $1.5/mo | **$4.4/mo** |
| 30 days retained | 211 GiB | 633 GiB ($9.5/mo) |

14 days is the right default for a home. It's enough to answer "what happened while I was away" and
you can always bump it. R2 charges nothing for egress, so watching footage is free; the only op cost
is a few thousand GETs a month, i.e. cents.

Full monthly bill: R2 ~$4.4 + droplet $6 + PlanetScale (existing instance, ~$0 incremental) ≈
**~$11/mo**.

---

## 9. Home-specific failure modes

| Failure | What actually happens | Handle it |
|---|---|---|
| WiFi drops for 30 s | gap in footage | spool + replay; gap marked on the timeline |
| Router reboot / site power cut | Pi boots, clock may be wrong | chrony before pipeline start; skewed windows get stored but flagged `suspect` |
| ISP outage for 2 h | spool fills | 1 GB ≈ 3 h; then drop oldest and record `dropped_s` |
| SD card dies from power cuts | node won't boot | read-only rootfs + `log2ram` + a spare pre-flashed card taped inside the case |
| Hub VM dies | all live views stop, no new chunks | systemd restart; state lives in PlanetScale + R2, nothing on the VM to restore |
| PlanetScale unreachable | chunk rows fail to write | chunks already landed in R2; retry upserts + `rebuild-catalog` — ingest never blocks on the DB |
| Camera silently freezes | you think it's fine, it isn't | "last fragment age > 2 min" alert — this is the only alert that matters |
| Someone unplugs the Pi | dead camera | Tailscale goes offline; alert on node offline |
| Neighbour's WiFi congestion | stutter | 5 GHz, or run Ethernet |
| Firmware update breaks capture | dead camera | keep the previous binary next to the new one, swap back in 30 s |

---

## 10. Build order

**Weekend 1 — prove the loop (do this before writing anything else).**
One Pi → GStreamer pipeline → a 100-line Go program that splits fragments and pushes them over a WS to
your laptop, which prints the size and latency of each. Then a static HTML page that plays them with
MSE. Success: live video in a browser, and you know your real per-frame latency. If this doesn't work,
nothing downstream matters.

**Weekend 2 — the hub.**
Move the receiver to a $6 DigitalOcean droplet (BLR1). Add the 5-min ring + fanout, the 10-min
chunker → R2 + Postgres, and Cloudflare Tunnel + Access. Success: you can open the app on your
phone over 4G, watch live, and scrub back an hour.

**Weekend 3 — locations 2 and 3.**
Flash two more cards from a script, join the tailnet, confirm in the UI. Add `/status` + the Telegram
alert. Success: a camera that dies at 3 a.m. tells you before you notice.

**Later, if you feel like it:** motion events (open-source `motion` or a TFLite model on the Pi —
skip the Hailo HAT), audio, per-camera bandwidth caps, and a `rebuild-catalog` run against an
empty database to prove the R2 durability story actually works.

---

## 11. Open questions

1. **"3 locations" — 3 houses, or 3 rooms?** If all three Pis are under one roof, skip the VM entirely:
   run the hub on a spare Pi 5 (or any always-on box) on the LAN, skip Tailscale, skip Cloudflare, and
   the running cost drops to **$4/mo of R2**. That's a materially different (and much simpler) build —
   it's the one question that changes the rest of this doc.
2. ~~**Do you already own the Pis?**~~ Answered: no — buying 3 Pi 4 kits, so the ~$345 BOM stands.
3. **Are all 3 locations in India?** BLR1 covers India. If one site is abroad the design still
   works — tailnet eats the distance, live view just gains RTT.
4. **Indoor or outdoor?** Outdoor changes the enclosure and the camera choice (a Pi in a box outdoors
   is a humidity problem; a $70 PoE camera is easier).
5. **Do you want family to view it?** If not, you can drop Cloudflare Access and live on the tailnet.
