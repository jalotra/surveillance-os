// live-socket — /live/{cam} WebSocket → MsePlayer (plans/frontend.md §2.2).
// Handles reconnect with capped backoff, init re-priming on every connect
// (the hub resends init + from the latest keyframe), and stall-triggered
// resync via MsePlayer.onStall.

import { decodeEnvelope } from "./proto";
import { codecFromInit } from "./mp4";
import { liveWsUrl } from "./config";
import { MsePlayer, mseSupported } from "./mse-player";

export type LiveState =
  | "connecting"
  | "live"
  | "waiting-keyframe"
  | "reconnecting"
  | "gap" // transient: footage lost, flashed then back to live
  | "unsupported"
  | "error";

export function connectLive(
  camId: string,
  video: HTMLVideoElement,
  onState: (s: LiveState, detail?: string) => void,
): () => void {
  let ws: WebSocket | null = null;
  let player: MsePlayer | null = null;
  let timer: number | null = null;
  let retries = 0;
  let closed = false;

  const ensurePlayer = (init: Uint8Array): boolean => {
    if (player) return true;
    const mime = `video/mp4; codecs="${codecFromInit(init)}"`;
    if (!mseSupported(mime)) {
      onState("unsupported", mime);
      return false;
    }
    player = new MsePlayer(video, mime, {
      live: true,
      onStall: () => {
        onState("waiting-keyframe");
        reconnect(true); // stall → fresh keyframe fastest via resubscribe
      },
    });
    return true;
  };

  const open = () => {
    if (closed) return;
    onState(retries ? "reconnecting" : "connecting");
    ws = new WebSocket(liveWsUrl(camId));
    ws.binaryType = "arraybuffer";

    ws.onopen = () => {
      retries = 0;
      onState("live");
    };
    ws.onmessage = (ev) => {
      const env = decodeEnvelope(ev.data as ArrayBuffer);
      if (!env) return;
      switch (env.header.type) {
        case "init":
          if (ensurePlayer(env.payload)) player!.push(env.payload);
          break;
        case "frag":
          player?.push(env.payload);
          break;
        case "gap": {
          onState("gap");
          window.setTimeout(() => onState("live"), 2500);
          break;
        }
      }
    };
    ws.onclose = () => reconnect(false);
    ws.onerror = () => ws?.close();
  };

  const reconnect = (immediate: boolean) => {
    if (closed) return;
    player?.reset(); // a new init arrives on resubscribe
    if (timer) clearTimeout(timer);
    const delay = immediate
      ? 0
      : Math.min(10_000, 1000 * Math.pow(2, retries++));
    timer = window.setTimeout(open, delay);
  };

  open();
  return () => {
    closed = true;
    if (timer) clearTimeout(timer);
    ws?.close();
    player?.destroy();
  };
}
