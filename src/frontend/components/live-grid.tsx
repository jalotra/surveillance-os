"use client";

import * as React from "react";
import { CameraCard } from "@/components/camera-card";
import { Skeleton } from "@/components/ui/skeleton";
import { fetchCameras } from "@/lib/api";
import type { Camera } from "@/lib/types";

export function LiveGrid() {
  const [cams, setCams] = React.useState<Camera[] | null>(null);
  const [err, setErr] = React.useState("");

  React.useEffect(() => {
    let dead = false;
    const poll = () =>
      fetchCameras()
        .then((c) => !dead && setCams(c))
        .catch((e) => !dead && setErr(String(e)));
    poll();
    const t = setInterval(poll, 5000);
    return () => {
      dead = true;
      clearInterval(t);
    };
  }, []);

  if (err)
    return (
      <div className="rounded-lg border border-destructive/40 bg-destructive/10 p-6 text-sm text-destructive">
        Hub unreachable: {err}
      </div>
    );

  if (cams === null)
    return (
      <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
        {[0, 1, 2].map((i) => (
          <Skeleton key={i} className="aspect-video w-full rounded-xl" />
        ))}
      </div>
    );

  if (cams.length === 0)
    return (
      <div className="rounded-lg border p-10 text-center text-sm text-muted-foreground">
        No cameras registered. Bring a Pi online to see it here.
      </div>
    );

  return (
    <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
      {cams.map((c) => (
        <CameraCard key={c.id} cam={c} />
      ))}
    </div>
  );
}
