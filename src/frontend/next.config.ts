import type { NextConfig } from "next";

// Dev convenience: proxy REST calls to the hub so the app behaves
// same-origin like it will behind Cloudflare Tunnel. The WebSocket is
// dialed directly (lib/config.ts) — Next rewrites don't reliably proxy
// WS upgrades.
const HUB = process.env.HUB_URL ?? "http://127.0.0.1:8080";

const nextConfig: NextConfig = {
  async rewrites() {
    return [
      { source: "/api/:path*", destination: `${HUB}/api/:path*` },
      { source: "/status", destination: `${HUB}/status` },
      { source: "/healthz", destination: `${HUB}/healthz` },
    ];
  },
};

export default nextConfig;
