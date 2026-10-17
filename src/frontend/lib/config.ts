// URL resolution. Everything the browser fetches is same-origin:
//   prod — one Cloudflare hostname fronts the app, /api, /status, /live
//   dev  — next.config.ts rewrites proxy /api + /status to the hub
//
// The viewer WebSocket can't ride Next's rewrite proxy, so it dials the
// hub directly in dev (ws://127.0.0.1:8080 — WS has no CORS). Override
// for non-standard dev setups with NEXT_PUBLIC_HUB_WS=ws://192.168.x.x:8080

const HUB_WS = process.env.NEXT_PUBLIC_HUB_WS;

function isDev(): boolean {
  return (
    typeof window !== "undefined" &&
    (window.location.hostname === "localhost" ||
      window.location.hostname === "127.0.0.1")
  );
}

export function apiUrl(path: string): string {
  return path; // same-origin always — rewrites (dev) or Tunnel (prod)
}

export function liveWsUrl(camId: string): string {
  if (HUB_WS) return HUB_WS.replace(/\/$/, "") + "/live/" + camId;
  if (isDev()) return "ws://127.0.0.1:8080/live/" + camId;
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}/live/${camId}`;
}
