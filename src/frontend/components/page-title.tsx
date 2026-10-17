"use client";

import { usePathname } from "next/navigation";

export function PageTitle() {
  const p = usePathname();
  const title = p.startsWith("/live/")
    ? `Live — ${decodeURIComponent(p.split("/")[2] ?? "")}`
    : p === "/live"
      ? "Live"
      : p.startsWith("/history")
        ? "History"
        : "Surveillance";
  return <h1 className="text-sm font-medium">{title}</h1>;
}
