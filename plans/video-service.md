# video-service — the Pi agent (one Go binary per Pi)

Capture → encode (hardware H.264) → split fMP4 → spool → push to hub over a tailnet WebSocket.
~400–600 lines of Go, one config file, one systemd unit. Reference: `docs/architecture.md` §5.

Repurposes `src/video-service/` — the Java/Spring service gets deleted. New Go module, no
shared deps with the hub except the wire protocol (copy the ~30 lines of message types;
independent builds beat a shared module here).

---

## 1. Interfaces

### 1.1 GStreamer subprocess

The agent does **not** link libcamera/GStreamer — it runs `gst-launch-1.0` as a child process
and reads fMP4 bytes from its stdout. Crash → restart with backoff; the spool is untouched.

```bash
# Pi 4 (hardware encoder, ~5% CPU)
gst-launch-1.0 -e libcamerasrc ! video/x-raw,width=1920,height=1080,framerate=10/1 \
  ! v4l2h264enc extra-controls="controls,video_bitrate=700000,video_gop_size=20" \
  ! h264parse config-interval=1 ! mp4mux fragment-duration=1000 ! fdsink fd=1

# Pi 5 fallback (software, ~15–20% CPU at 1080p10)
... ! x264enc tune=zerolatency speed-preset=ultrafast bitrate=700 key-int-max=20 ! ...

# Optional thumbnail branch (for the grid view): tee → jpegenc every 5 s
tee name=v ! queue ! v4l2h264enc ... ! mp4mux ... ! fdsink fd=1 \
  v. ! queue ! videorate ! video/x-raw,framerate=1/5 ! jpegenc ! fdsink fd=3
```

Before shipping: verify encoder controls with `v4l2-ctl -d /dev/video11 --list-ctrls` — a bogus
control value fails at runtime, not parse time.

### 1.2 Uplink WebSocket — `wss://<hub-tailnet-ip>:8080/ingest`

Producer side of the protocol spec in `plans/backend.md` §1.1. Same binary envelope:
`[u32 header_len][header JSON][payload]`. Message order on connect:

1. `hello` → wait `hello_ok{resume_from_seq}`
2. if `resume_from_seq` < oldest needed: replay spool (see §2.2), `replay=1` on headers
3. `init` once, then live `frag` messages; `thumb` every 5 s; `stat` every 15 s

Reconnect: exponential backoff 1 s → 30 s, `tailscale status` check first (don't burn attempts
when the tailnet is down — that's what the spool is for).

**Never block capture.** The pipeline feeds spool unconditionally; uplink is a consumer, not a
gate. If `ack` stalls, keep spooling.

### 1.3 Config — `/etc/surveillance/agent.toml`

```toml
cam_id   = "home-a"            # must match cameras.id + tailnet name on the hub
hub      = "100.x.y.z:8080"    # hub's tailnet IP
source   = "libcamerasrc"      # gst source element; "videotestsrc is-live=true"
                             # fakes a camera on any gst host (dev/e2e/bring-up)
width = 1920; height = 1080; fps = 10; bitrate = 700_000
software_encode = false        # x264enc instead of v4l2h264enc (Pi 5 / non-Pi hosts)
spool_dir = "/run/surveillance/spool"   # tmpfs, see §2.2
spool_max_mb = 1024
thumbs = true
```

### 1.4 systemd

```ini
[Unit] Description=surveillance agent; After=chrony-wait.service tailscaled.service
[Service] ExecStart=/opt/surveillance/agent -config /etc/surveillance/agent.toml
         Restart=always; RestartSec=3; MemoryMax=1536M  # cgroup v2 charges the
                                                     # 1GB tmpfs spool to us
```

Chrony ordering matters — no timestamp is trustworthy before NTP sync.

---

## 2. Data models

### 2.1 Fragment

```go
type Fragment struct {
    Seq   uint64   // monotonic per boot_id
    PtsMs int64    // wall-clock ms at fragment start (not gst PTS — stamped when moof seen)
    DurMs uint32
    Key   bool     // fragment begins with keyframe
    Data  []byte   // moof+mdat
}
```

`boot_id` = `/proc/sys/kernel/random/boot_id` at agent start, sent in `hello`. It scopes the
seq counter and anchors the hub's clock-skew check.

### 2.2 Spool — segment files, not an in-place ring

```
/run/surveillance/spool/
  index                 -- json: {boot_id, write_seg, read_seg, read_off, dropped_s}
  seg-000042.spl        -- ~16 MB each
```

Segment = `[u32 magic 'SPLF'][record]*`; record = fixed 32 B header + payload:

```
magic u32 'SFRG' | ver u16 | flags u16 (bit0=key) | seq u64 | pts_ms i64 | dur_ms u32 | len u32 | crc32 u32
```

- tmpfs, not SD — 1 GB cap on a 4 GB Pi 4, zero card wear, and anything lost to a power cut was
  un-uploaded anyway.
- Over cap → delete oldest segment, add its duration to `dropped_s` (shipped in `stat`, stored
  on the hub as a gap event).
- `ack{last_seq}` trims segments strictly older than it.
- Replay: sequential read from oldest un-acked. Interleave with live (e.g. 4 spool frags : 1
  live) so a 3 h backlog doesn't freeze the live view.

### 2.3 Status payload

```json
{"type":"stat","uptime_s":1234,"temp_c":51.2,"throttled":"0x0",
 "spool_bytes":1234567,"dropped_s_total":0,"wifi_rssi":-58}
```

`throttled` from `vcgencmd get_throttled` — non-zero = under-voltage/thermal, hub alerts on it.

---

## 3. Code model

```
src/video-service/                    -- new Go module (module surveillance-os/agent)
  cmd/agent/main.go
  internal/
    config/   toml load + validate                        (~60 lines)
    gst/      pipeline string build, exec, stdout reader, restart watchdog   (~120)
    fmp4/     box splitter                                (~120)
    spool/    segment writer/reader/trimmer               (~200)
    uplink/   ws dial, hello handshake, send loop, replay (~200)
    health/   vcgencmd, temp, rssi                        (~60)
    proto/    envelope + message types (copied from hub)  (~80)
```

### The splitter is the only tricky part

`mp4mux` stdout is a byte stream of MP4 boxes: `ftyp`+`moov` once, then `moof`+`mdat` per
fragment. Walk box headers (`[u32 len][4cc type]`):

```go
type Splitter struct{ r *bufio.Reader }
func (s *Splitter) ReadInit() ([]byte, error)            // consume ftyp+moov once
func (s *Splitter) Next() (Fragment, error)              // buffer through mdat; key = moof.traf flags
```

Keyframe detection: parse `moof` → `trun` flags (`sample_is_non_sync_sample` absent on sample
0) — or simpler, `mfra`-free heuristic: every `fragment-duration` boundary after a GOP edge.
Simplest reliable: check first trun's `sample_flags` word; ~20 lines of box walking.

### Key signatures

```go
func Run(ctx, cfg Config) error                // supervisor: gst → splitter → {spool, uplink}
func (u *Uplink) Connect(ctx) error            // dial+hello+replay+live
func (u *Uplink) Send(f Fragment) error        // enqueue; never blocks capture path
func (sp *Spool) Write(f Fragment)             // append, evict-oldest if over cap
func (sp *Spool) ReadFrom(seq uint64) iter.Seq[Fragment]
```

### Runtime knobs (from architecture §7)

`GOGC=200`, `GOMEMLIMIT=256MiB`, `GOMAXPROCS=4` via systemd `Environment=`. Reuse a
`sync.Pool` of 256 KB buffers — one fragment is ~87 KB at 0.7 Mbps.

---

## 4. Build order (architecture §10, weekend 1)

1. `gst` + `fmp4` + a `main` that pipes fragments to `/dev/null` with per-frag size/timing logs —
   proves the pipeline and splitter before any networking exists.
2. `uplink` to the laptop receiver; live video in a browser via MSE.
3. `spool` + replay — pull the Pi's wifi for 5 min, confirm the gap fills in on reconnect.
4. `health`/`stat` + thumbnails + the install script for SD cards 2 and 3.

## 5. Out of scope

Camera control beyond the fixed pipeline (no PTZ, no exposure UI), OTA/self-update (do
`scp` + `systemctl restart`), audio, motion detection — all listed as "later" in the doc.
