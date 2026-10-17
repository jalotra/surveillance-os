"use client";

import * as React from "react";
import type { Chunk, HubEvent } from "@/lib/types";
import { cn } from "@/lib/utils";

// 24h strip: chunks as blocks (amber = suspect), events as red ticks,
// click anywhere to pick a start time (plans/frontend.md §4).
export function Timeline({
  dayStartS,
  chunks,
  events,
  cursorMs,
  onPick,
}: {
  dayStartS: number; // unix seconds, local midnight
  chunks: Chunk[];
  events: HubEvent[];
  cursorMs: number | null;
  onPick: (wallMs: number) => void;
}) {
  const ref = React.useRef<HTMLDivElement>(null);
  const DAY = 86400;

  const pct = (secInDay: number) => `${(secInDay / DAY) * 100}%`;

  const pick = (e: React.MouseEvent) => {
    const r = ref.current!.getBoundingClientRect();
    const frac = Math.min(0.999, Math.max(0, (e.clientX - r.left) / r.width));
    onPick((dayStartS + frac * DAY) * 1000);
  };

  const hours = Array.from({ length: 25 }, (_, i) => i);

  return (
    <div className="select-none">
      <div
        ref={ref}
        onClick={pick}
        className="relative h-12 w-full cursor-crosshair rounded-md border bg-muted/40"
      >
        {/* hour gridlines */}
        {hours.map((h) => (
          <div
            key={h}
            className="absolute top-0 h-full w-px bg-border/60"
            style={{ left: pct(h * 3600) }}
          />
        ))}

        {/* chunk blocks */}
        {chunks.map((c, i) => {
          const s = Math.max(0, c.window_start - dayStartS);
          const e = Math.min(DAY, c.window_end - dayStartS);
          if (e <= s) return null;
          return (
            <div
              key={i}
              title={`${new Date(c.window_start * 1000).toLocaleTimeString()} (${(c.bytes / 1048576).toFixed(1)}MB)`}
              className={cn(
                "absolute top-1 h-[calc(100%-8px)] rounded-sm",
                c.suspect ? "bg-amber-500/80" : "bg-emerald-600/80",
                c.dropped_s > 0 && "border border-red-500/60",
              )}
              style={{ left: pct(s), width: `max(2px, ${pct(e - s)})` }}
            />
          );
        })}

        {/* event ticks */}
        {events.map((ev) => {
          const s = new Date(ev.ts).getTime() / 1000 - dayStartS;
          if (s < 0 || s > DAY) return null;
          return (
            <div
              key={ev.id}
              title={`${ev.kind} ${new Date(ev.ts).toLocaleTimeString()}`}
              className={cn(
                "absolute bottom-0 h-3 w-0.5",
                ev.kind === "gap" ? "bg-red-500" : "bg-orange-400",
              )}
              style={{ left: pct(s) }}
            />
          );
        })}

        {/* playhead */}
        {cursorMs != null && (
          <div
            className="absolute top-0 h-full w-0.5 bg-foreground"
            style={{ left: pct(cursorMs / 1000 - dayStartS) }}
          />
        )}
      </div>
      <div className="mt-1 flex justify-between text-[10px] text-muted-foreground">
        {[0, 4, 8, 12, 16, 20, 24].map((h) => (
          <span key={h}>{String(h).padStart(2, "0")}:00</span>
        ))}
      </div>
    </div>
  );
}
