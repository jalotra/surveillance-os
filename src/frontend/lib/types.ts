// Client-side types mirroring the hub's JSON output (internal/api) and the
// WebSocket envelope headers (internal/proto). See plans/frontend.md §3.

export interface Camera {
  id: string;
  site: string;
  status: "online" | "offline";
  last_seen: string; // RFC3339
  temp_c: number;
  spool_bytes: number;
  thumb_url: string;
}

export interface Chunk {
  window_start: number; // unix seconds
  window_end: number;
  bytes: number;
  duration_s: number;
  dropped_s: number;
  suspect: boolean;
}

// Sidecar written next to each chunk ({cam}/{idx}.json in R2).
// keyframes[].off is the absolute byte offset inside the .mp4 —
// the init segment is bytes 0..keyframes[0].off-1.
export interface Manifest {
  cam: string;
  idx: number;
  codec: string;
  codec_str: string;
  w: number;
  h: number;
  keyframes: { t_ms: number; off: number }[]; // t_ms relative to window start
  seqs: string[];
  last_seq: number;
  last_end_ms: number;
  dropped_s: number;
  suspect: boolean;
  finalized: boolean;
}

export interface HubEvent {
  id: number;
  cam_id: string;
  ts: string;
  kind: "gap" | "silence" | "suspect_clock" | "offline" | "online";
  meta: Record<string, unknown>;
}

export interface HubStatus {
  uptime_s: number;
  goroutines: number;
  mem_alloc_mb: number;
  mem_sys_mb: number;
  num_cpu: number;
  go_version: string;
  time: string;
  cameras: {
    id: string;
    online: boolean;
    last_frag_age_s: number;
    ring_frags: number;
    ring_bytes: number;
    ring_subs: number;
    suspect: boolean;
  }[];
}

// ---- wire messages (hub→viewer on /live/{cam}) ----

export interface FragHeader {
  type: "frag";
  seq: number;
  pts_ms: number;
  dur_ms: number;
  key: number; // 0/1
  replay: number; // 0/1
}

export interface GapHeader {
  type: "gap";
  seq?: number;
  dropped_s?: number;
}

export type WireHeader =
  | { type: "init" }
  | FragHeader
  | GapHeader
  | { type: string; [k: string]: unknown };
