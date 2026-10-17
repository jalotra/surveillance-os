// MsePlayer — MediaSource wrapper for the fMP4 fragment stream
// (plans/frontend.md §2.2). Rules enforced here:
//   - never appendBuffer while sb.updating — everything queues
//   - live edge chasing: >2s behind → slight catch-up rate, >4s → skip
//   - prune old buffer so long-running views don't grow memory
//   - 'waiting' > 2.5s fires onStall → caller reconnects for a fresh keyframe

export type StallCallback = () => void;

const MSE: typeof MediaSource | undefined =
  typeof window !== "undefined"
    ? ((window as { ManagedMediaSource?: typeof MediaSource }).ManagedMediaSource ??
      window.MediaSource)
    : undefined;

export function mseSupported(mime: string): boolean {
  try {
    return !!MSE && MSE.isTypeSupported(mime);
  } catch {
    return false;
  }
}

export class MsePlayer {
  private video: HTMLVideoElement;
  private mime: string;
  private live: boolean;
  private onStall?: StallCallback;
  private ms: MediaSource | null = null;
  private sb: SourceBuffer | null = null;
  private queue: Uint8Array[] = [];
  private objectUrl: string | null = null;
  private watchdog: number | null = null;
  private stallSince = 0;
  private destroyed = false;

  constructor(
    video: HTMLVideoElement,
    mime: string,
    opts: { onStall?: StallCallback; live?: boolean } = {},
  ) {
    this.video = video;
    this.mime = mime;
    this.live = opts.live ?? false;
    this.onStall = opts.onStall;
    this.open();
  }

  private open() {
    if (!MSE) return;
    this.ms = new MSE();
    this.objectUrl = URL.createObjectURL(this.ms);
    this.video.src = this.objectUrl;
    this.ms.addEventListener("sourceopen", () => {
      if (this.destroyed || !this.ms) return;
      try {
        this.sb = this.ms.addSourceBuffer(this.mime);
        this.sb.mode = "segments";
        this.sb.addEventListener("updateend", () => this.drain());
        this.drain();
      } catch {
        /* codec unsupported — surface via onStall-less state */
      }
    });
    this.video.addEventListener("waiting", this.onWaiting);
    this.video.addEventListener("playing", this.onPlaying);
    this.watchdog = window.setInterval(() => this.tick(), 500);
  }

  private onWaiting = () => {
    if (!this.stallSince) this.stallSince = Date.now();
  };
  private onPlaying = () => {
    this.stallSince = 0;
  };

  private tick() {
    if (this.destroyed) return;
    const v = this.video;
    if (this.stallSince && Date.now() - this.stallSince > 2500) {
      this.stallSince = 0;
      this.onStall?.();
      return;
    }
    if (v.buffered.length === 0) return;
    const end = v.buffered.end(v.buffered.length - 1);
    const behind = end - v.currentTime;
    if (this.live) {
      if (behind > 4) {
        v.currentTime = Math.max(0, end - 0.5); // hard skip to the edge
        v.playbackRate = 1;
      } else if (behind > 2) {
        v.playbackRate = 1.08; // gentle catch-up
      } else {
        v.playbackRate = 1;
      }
    }
    // Prune anything >8s behind the playhead (skip while appending).
    if (this.sb && !this.sb.updating && v.buffered.length > 0) {
      const start = v.buffered.start(0);
      if (v.currentTime - start > 8) {
        try {
          this.sb.remove(start, v.currentTime - 4);
        } catch {
          /* remove races are harmless */
        }
      }
    }
  }

  private drain() {
    if (!this.sb || this.sb.updating || this.queue.length === 0) return;
    const next = this.queue.shift()!;
    try {
      this.sb.appendBuffer(next.buffer.slice(next.byteOffset, next.byteOffset + next.byteLength) as ArrayBuffer);
    } catch {
      // Quota / decode error — drop the fragment, keep going.
    }
  }

  /** Queue bytes for append. init must be the first push after (re)open. */
  push(bytes: Uint8Array) {
    if (this.destroyed) return;
    this.queue.push(bytes);
    this.drain();
  }

  /** Fresh MediaSource — used on reconnect so a new init lands cleanly. */
  reset() {
    this.queue = [];
    this.sb = null;
    this.ms?.removeEventListener("sourceopen", () => {});
    if (this.objectUrl) URL.revokeObjectURL(this.objectUrl);
    this.ms = null;
    this.video.removeAttribute("src");
    this.video.load();
    this.open();
  }

  destroy() {
    this.destroyed = true;
    if (this.watchdog) clearInterval(this.watchdog);
    this.queue = [];
    if (this.objectUrl) URL.revokeObjectURL(this.objectUrl);
    this.video.removeEventListener("waiting", this.onWaiting);
    this.video.removeEventListener("playing", this.onPlaying);
    this.video.removeAttribute("src");
    this.video.load();
  }
}
