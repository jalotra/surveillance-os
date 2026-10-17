"use client";

import * as React from "react";
import { Badge } from "@/components/ui/badge";
import { Loader2, VideoOff } from "lucide-react";
import { connectLive, type LiveState } from "@/lib/live-socket";

// One camera's live view: WS → decode envelope → MsePlayer → <video>.
// Shows the real connection states — connecting / waiting-keyframe /
// gap / reconnecting — instead of a silent black frame.
export function LiveVideo({ camId }: { camId: string }) {
  const videoRef = React.useRef<HTMLVideoElement>(null);
  const [state, setState] = React.useState<LiveState>("connecting");
  const [detail, setDetail] = React.useState("");

  React.useEffect(() => {
    const v = videoRef.current;
    if (!v) return;
    v.muted = true;
    v.play().catch(() => {});
    return connectLive(camId, v, (s, d) => {
      setState(s);
      setDetail(d ?? "");
    });
  }, [camId]);

  const overlay = (() => {
    switch (state) {
      case "live":
        return null;
      case "gap":
        return { icon: <VideoOff className="size-8" />, text: "footage gap — resuming" };
      case "unsupported":
        return {
          icon: <VideoOff className="size-8" />,
          text: `this browser can't play live (${detail || "no MSE"}) — use history`,
        };
      case "error":
        return { icon: <VideoOff className="size-8" />, text: detail || "stream error" };
      default:
        return {
          icon: <Loader2 className="size-8 animate-spin" />,
          text:
            state === "waiting-keyframe"
              ? "waiting for keyframe…"
              : state === "reconnecting"
                ? "reconnecting…"
                : "connecting…",
        };
    }
  })();

  return (
    <div className="relative aspect-video w-full overflow-hidden rounded-xl bg-black">
      {/* eslint-disable-next-line jsx-a11y/media-has-caption */}
      <video ref={videoRef} className="h-full w-full" playsInline muted autoPlay />
      <Badge
        className={`absolute right-3 top-3 ${
          state === "live" ? "bg-red-600 text-white" : "bg-muted text-muted-foreground"
        }`}
      >
        {state === "live" ? "LIVE" : state === "gap" ? "GAP" : "…"}
      </Badge>
      {overlay && (
        <div className="absolute inset-0 flex flex-col items-center justify-center gap-2 bg-black/60 text-sm text-muted-foreground">
          {overlay.icon}
          <span>{overlay.text}</span>
        </div>
      )}
    </div>
  );
}
