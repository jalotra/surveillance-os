"use client";

import * as React from "react";
import Link from "next/link";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { VideoOff } from "lucide-react";
import { apiUrl } from "@/lib/config";
import type { Camera } from "@/lib/types";

// Grid card: the hub's latest JPEG, refreshed on a poll — near-live without
// burning an MSE pipeline per card (plans/frontend.md §4).
export function CameraCard({ cam }: { cam: Camera }) {
  const [tick, setTick] = React.useState(0);
  const [imgOk, setImgOk] = React.useState(true);

  React.useEffect(() => {
    const t = setInterval(() => setTick((n) => n + 1), 2000);
    return () => clearInterval(t);
  }, []);

  const online = cam.status === "online";
  const src = `${apiUrl(cam.thumb_url)}?t=${tick}`;

  return (
    <Link href={`/live/${cam.id}`} className="group">
      <Card className="overflow-hidden transition-colors hover:border-foreground/30">
        <div className="relative aspect-video w-full bg-black">
          {imgOk ? (
            // eslint-disable-next-line @next/next/no-img-element
            <img
              src={src}
              alt={cam.id}
              className="h-full w-full object-cover"
              onError={() => setImgOk(false)}
              onLoad={() => setImgOk(true)}
            />
          ) : (
            <div className="flex h-full w-full items-center justify-center text-muted-foreground">
              <VideoOff className="size-8 opacity-40" />
            </div>
          )}
          <Badge
            className={`absolute right-2 top-2 ${
              online
                ? "bg-red-600 text-white"
                : "bg-muted text-muted-foreground"
            }`}
          >
            {online ? "LIVE" : "OFFLINE"}
          </Badge>
        </div>
        <CardHeader className="p-3">
          <div className="flex items-center justify-between">
            <CardTitle className="text-sm font-medium">{cam.id}</CardTitle>
            <span className="text-xs text-muted-foreground">{cam.site}</span>
          </div>
        </CardHeader>
        <CardContent className="px-3 pb-3 pt-0">
          <div className="flex gap-3 text-[11px] text-muted-foreground">
            {cam.temp_c > 0 && <span>{cam.temp_c.toFixed(0)}°C</span>}
            {cam.spool_bytes > 0 && (
              <span>spool {(cam.spool_bytes / 1048576).toFixed(0)}MB</span>
            )}
            <span className="ml-auto">
              {cam.last_seen.startsWith("0001")
                ? "never seen"
                : `seen ${new Date(cam.last_seen).toLocaleTimeString()}`}
            </span>
          </div>
        </CardContent>
      </Card>
    </Link>
  );
}
