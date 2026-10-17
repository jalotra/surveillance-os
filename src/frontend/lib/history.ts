// history — timeline playback via manifest keyframe offsets + Range GETs
// (plans/frontend.md §2.2, §3). One HistorySession owns one <video>:
//   open(cam, wallClockMs) → find covering 10-min chunk → presigned urls →
//   manifest → Range-fetch init + keyframe frags → MsePlayer.
// The wall-clock↔media-time offset is learned from the first appended
// fragment's buffered.start — we never guess tfdt internals.

import { fetchChunks, fetchChunkURLs, fetchManifest, fetchRange } from "./api";
import { codecFromInit } from "./mp4";
import { MsePlayer } from "./mse-player";
import type { Chunk, Manifest } from "./types";

export type HistoryState = "loading" | "playing" | "ended" | "error";

const CHUNK_MS = 600_000;
const BUFFER_AHEAD_S = 15;

export class HistorySession {
  private video: HTMLVideoElement;
  private player: MsePlayer;
  private onState: (s: HistoryState, detail?: string) => void;
  private onTime: (wallMs: number) => void;

  private cam: string;
  private chunks: Chunk[] = [];
  private ci = -1;
  private man: Manifest | null = null;
  private mp4Url = "";
  private fetchIdx = 0; // next manifest.keyframes[] entry to fetch
  private mediaBaseMs = 0; // buffered.start + this = wall clock
  private chunkWallStartMs = 0;
  private chunkWallEndMs = 0;
  private pumpTimer: number | null = null;
  private ended = false;
  private destroyed = false;

  static async open(
    cam: string,
    atMs: number,
    video: HTMLVideoElement,
    onState: (s: HistoryState, d?: string) => void,
    onTime: (wallMs: number) => void,
  ): Promise<HistorySession> {
    const s = new HistorySession(cam, video, onState, onTime);
    await s.seek(atMs);
    s.startPump();
    return s;
  }

  private constructor(
    cam: string,
    video: HTMLVideoElement,
    onState: (s: HistoryState, d?: string) => void,
    onTime: (wallMs: number) => void,
  ) {
    this.cam = cam;
    this.video = video;
    this.onState = onState;
    this.onTime = onTime;
    this.player = new MsePlayer(video, "video/mp4"); // mime refined per chunk via codecFromInit
    video.addEventListener("timeupdate", () => {
      if (!this.destroyed) this.onTime(this.wallNow());
    });
  }

  private wallNow(): number {
    return this.mediaBaseMs + this.video.currentTime * 1000;
  }

  /** Jump to an absolute wall-clock ms — same or different chunk. */
  async seek(atMs: number) {
    this.onState("loading");
    this.ended = false;
    try {
      if (this.chunks.length === 0) {
        this.chunks = await fetchChunks(this.cam, Math.floor(atMs / 1000) - 86_400, Math.floor(atMs / 1000) + 600);
      }
      const ws = Math.floor(atMs / 1000);
      const ci = this.chunks.findIndex((c) => c.window_start <= ws && ws < c.window_end);
      if (ci < 0) {
        this.onState("error", "no footage at that time");
        return;
      }
      await this.openChunk(ci, atMs);
    } catch (e) {
      this.onState("error", String(e));
    }
  }

  private async openChunk(ci: number, atMs: number) {
    this.ci = ci;
    const c = this.chunks[ci];
    this.chunkWallStartMs = c.window_start * 1000;
    this.chunkWallEndMs = c.window_end * 1000;

    const urls = await fetchChunkURLs(this.cam, c.window_start);
    this.man = await fetchManifest(urls.manifest_url);
    this.mp4Url = urls.mp4_url;
    const kfs = this.man.keyframes;
    if (kfs.length === 0) throw new Error("manifest has no keyframes");

    // init segment = bytes before the first keyframe offset.
    const init = await fetchRange(this.mp4Url, 0, kfs[0].off - 1);

    // Recreate the player under the real codec when it changes/starts.
    this.player.destroy();
    this.player = new MsePlayer(this.video, `video/mp4; codecs="${codecFromInit(init)}"`);
    // Wait for the SourceBuffer before pushing.
    await new Promise<void>((res) => setTimeout(res, 60));
    this.player.push(init);

    // nearest keyframe at or before the requested wall time
    const rel = atMs - this.chunkWallStartMs;
    let k = 0;
    for (let i = 0; i < kfs.length; i++) if (kfs[i].t_ms <= rel) k = i;
    this.fetchIdx = k;
    await this.fetchAndPush(k);

    // Learn media↔wall offset, then land on the requested time.
    for (let i = 0; i < 40 && this.video.buffered.length === 0; i++) {
      await new Promise((r) => setTimeout(r, 50));
    }
    if (this.video.buffered.length > 0) {
      const mediaStartMs = this.video.buffered.start(0) * 1000;
      this.mediaBaseMs = this.chunkWallStartMs + kfs[k].t_ms - mediaStartMs;
      this.video.currentTime = Math.max(0, (atMs - this.mediaBaseMs) / 1000);
    }
    this.video.play().catch(() => {});
    this.onState("playing");
  }

  private async fetchAndPush(kfIdx: number) {
    const kfs = this.man!.keyframes;
    if (kfIdx >= kfs.length) return;
    const off = kfs[kfIdx].off;
    const end = kfIdx + 1 < kfs.length ? kfs[kfIdx + 1].off - 1 : undefined;
    const frag = await fetchRange(this.mp4Url, off, end);
    this.player.push(frag);
    this.fetchIdx = kfIdx + 1;
  }

  private startPump() {
    const tick = async () => {
      if (this.destroyed || this.ended) return;
      try {
        // top up ahead of the playhead
        if (this.man && this.video.buffered.length > 0) {
          const end = this.video.buffered.end(this.video.buffered.length - 1);
          const ahead = end - this.video.currentTime;
          if (ahead < BUFFER_AHEAD_S && this.fetchIdx < this.man.keyframes.length) {
            await this.fetchAndPush(this.fetchIdx);
          }
        }
        // boundary crossing → next chunk
        if (this.wallNow() >= this.chunkWallEndMs - 500) {
          const next = this.chunks.findIndex((c) => c.window_start >= this.chunkWallEndMs / 1000 - 1);
          if (next >= 0 && next !== this.ci) {
            await this.openChunk(next, this.chunkWallEndMs + 500);
          } else {
            this.ended = true;
            this.onState("ended");
            return;
          }
        }
      } catch {
        /* keep pumping; transient range errors are fine */
      }
      this.pumpTimer = window.setTimeout(tick, 500);
    };
    this.pumpTimer = window.setTimeout(tick, 500);
  }

  destroy() {
    this.destroyed = true;
    if (this.pumpTimer) clearTimeout(this.pumpTimer);
    this.player.destroy();
  }
}

export function windowStartOf(atMs: number): number {
  return Math.floor(atMs / CHUNK_MS) * (CHUNK_MS / 1000);
}
