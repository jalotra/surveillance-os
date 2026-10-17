#!/usr/bin/env bash
# e2e-local.sh — run the REAL agent against the REAL hub on this machine.
#
# Two capture modes, picked automatically:
#   • real gstreamer installed → agent.toml uses source="videotestsrc
#     is-live=true" + software_encode → real x264enc/mp4mux fMP4 that is
#     actually decodable (plus the fd-3 jpeg thumbnail branch).
#   • no gstreamer → a shim named gst-launch-1.0 execs cmd/fakecam, which
#     emits structurally-valid but undecodable fMP4.
#
# Either way this exercises: gst subprocess → splitter → spool → uplink →
# ingest → ring → chunker → finalize → sidecar + R2 put — everything
# except real hardware, tailscale whois, and real R2.
#
#   bash scripts/e2e-local.sh
set -euo pipefail
cd "$(dirname "$0")/.."

WORK="$(mktemp -d)"
HUB_LOG="$WORK/hub.log"; AGENT_LOG="$WORK/agent.log"
HUB_PID=""; AGENT_PID=""
cleanup() {
  [ -n "$AGENT_PID" ] && kill "$AGENT_PID" 2>/dev/null || true
  [ -n "$HUB_PID" ] && kill "$HUB_PID" 2>/dev/null || true
  echo "--- hub log (tail) ---"; tail -20 "$HUB_LOG" || true
  echo "--- agent log (tail) ---"; tail -20 "$AGENT_LOG" || true
  echo "workdir kept: $WORK"
}
trap cleanup EXIT

REAL_GST=0
if command -v gst-launch-1.0 >/dev/null 2>&1 && \
   gst-inspect-1.0 videotestsrc >/dev/null 2>&1 && \
   gst-inspect-1.0 x264enc >/dev/null 2>&1 && \
   gst-inspect-1.0 mp4mux >/dev/null 2>&1; then
  REAL_GST=1
fi

echo "==> building"
(cd src/backend && go build -o "$WORK/hub" ./cmd/hub)
(cd src/video-service && go build -o "$WORK/agent" ./cmd/agent)

AGENT_PATH="$PATH"
if [ "$REAL_GST" -eq 1 ]; then
  echo "==> capture: real gstreamer (videotestsrc → x264enc → mp4mux)"
  cat > "$WORK/agent.toml" <<EOF
cam_id = "dev-cam"
hub    = "127.0.0.1:18080"
source = "videotestsrc is-live=true pattern=ball"
software_encode = true
thumbs = true
width = 1280
height = 720
fps = 10
bitrate = 700000
spool_dir = "$WORK/spool"
spool_max_mb = 64
EOF
else
  echo "==> capture: fakecam shim (no gstreamer found)"
  (cd src/video-service && go build -o "$WORK/fakecam" ./cmd/fakecam)
  mkdir -p "$WORK/bin"
  printf '#!/bin/sh\nexec "%s/fakecam"\n' "$WORK" > "$WORK/bin/gst-launch-1.0"
  chmod +x "$WORK/bin/gst-launch-1.0"
  AGENT_PATH="$WORK/bin:$PATH"
  cat > "$WORK/agent.toml" <<EOF
cam_id = "dev-cam"
hub    = "127.0.0.1:18080"
thumbs = false
width = 1920
height = 1080
fps = 10
bitrate = 700000
spool_dir = "$WORK/spool"
spool_max_mb = 64
EOF
fi

echo "==> starting hub (dev mode: INGEST_AUTH=none, discard blob, null store)"
INGEST_AUTH=none LISTEN_ADDR=127.0.0.1:18080 \
  CHUNK_SCRATCH_DIR="$WORK/scratch" R2_PREFIX=surveillance \
  "$WORK/hub" >"$HUB_LOG" 2>&1 &
HUB_PID=$!
sleep 1

echo "==> starting agent"
PATH="$AGENT_PATH" "$WORK/agent" -config "$WORK/agent.toml" >"$AGENT_LOG" 2>&1 &
AGENT_PID=$!

echo "==> streaming ~12s"
sleep 12

echo "==> checks"
FAIL=0
chk() { if eval "$2"; then echo "  PASS: $1"; else echo "  FAIL: $1"; FAIL=1; fi; }

chk "camera registered+online" \
  "curl -sf http://127.0.0.1:18080/api/cameras | grep -q 'dev-cam'"
chk "ring receiving fragments" \
  "curl -sf http://127.0.0.1:18080/status | grep -q '\"ring_frags\":[1-9]'"
if [ "$REAL_GST" -eq 1 ]; then
  chk "jpeg thumbnail served" \
    "curl -sf http://127.0.0.1:18080/api/thumb/dev-cam | head -c2 | od -An -tx1 | tr -d ' \n' | grep -q 'ffd8'"
fi

echo "==> killing agent (disconnect → Flush should finalize open window)"
kill "$AGENT_PID" 2>/dev/null || true; AGENT_PID=""
sleep 2

chk "window sidecar written" \
  "ls '$WORK/scratch/dev-cam'/*.json >/dev/null 2>&1"
chk "finalize put attempted (discard blob logs it)" \
  "grep -q 'blob.Discard: put surveillance/dev-cam/.*\.mp4' '$HUB_LOG'"
chk "manifest put attempted" \
  "grep -q 'blob.Discard: put surveillance/dev-cam/.*\.json' '$HUB_LOG'"
if [ "$REAL_GST" -eq 1 ]; then
  chk "manifest records real codec+dimensions" \
    "grep -Eq '\"codec_str\":\"avc1' '$WORK'/scratch/dev-cam/*.json && grep -Eq '\"w\":1280' '$WORK'/scratch/dev-cam/*.json"
fi
chk "camera went offline" \
  "curl -sf http://127.0.0.1:18080/api/cameras | grep -q '\"status\":\"offline\"'"

kill "$HUB_PID" 2>/dev/null || true; HUB_PID=""
echo
if [ "$FAIL" -eq 0 ]; then echo "E2E PASS"; else echo "E2E FAIL — see logs above"; exit 1; fi
