// Typed REST calls against the hub (plans/frontend.md §2.1).
// Same-origin through Cloudflare Access in prod; hub-direct in dev.

import { apiUrl } from "./config";
import type { Camera, Chunk, HubEvent, HubStatus, Manifest } from "./types";

async function get<T>(path: string): Promise<T> {
  const r = await fetch(apiUrl(path), { cache: "no-store" });
  if (!r.ok) throw new Error(`${path}: ${r.status}`);
  return r.json() as Promise<T>;
}

export const fetchCameras = () => get<Camera[]>("/api/cameras");

export const fetchStatus = () => get<HubStatus>("/status");

export function fetchChunks(cam: string, fromS: number, toS: number) {
  return get<Chunk[]>(`/api/chunks?cam=${cam}&from=${fromS}&to=${toS}`);
}

export function fetchEvents(cam: string, fromS: number, toS: number, limit = 500) {
  return get<HubEvent[]>(`/api/events?cam=${cam}&from=${fromS}&to=${toS}&limit=${limit}`);
}

export interface ChunkURLs {
  manifest_url: string;
  mp4_url: string;
}

export function fetchChunkURLs(cam: string, windowS: number) {
  return get<ChunkURLs>(`/api/chunks/${cam}/${windowS}/urls`);
}

// Absolute or relative URL — presigned R2 in prod, /api/dev/object in dev.
export async function fetchManifest(url: string): Promise<Manifest> {
  const r = await fetch(apiUrl(url), { cache: "no-store" });
  if (!r.ok) throw new Error(`manifest: ${r.status}`);
  return r.json();
}

export async function fetchRange(url: string, start: number, end?: number): Promise<Uint8Array> {
  const r = await fetch(apiUrl(url), {
    headers: { Range: `bytes=${start}-${end ?? ""}` },
    cache: "no-store",
  });
  if (!r.ok && r.status !== 206) throw new Error(`range ${start}: ${r.status}`);
  return new Uint8Array(await r.arrayBuffer());
}
