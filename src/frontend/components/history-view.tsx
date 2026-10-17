"use client";

import * as React from "react";
import { useSearchParams } from "next/navigation";
import { Loader2, VideoOff } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Timeline } from "@/components/timeline";
import { fetchCameras, fetchChunks, fetchEvents } from "@/lib/api";
import { HistorySession, type HistoryState } from "@/lib/history";
import type { Camera, Chunk, HubEvent } from "@/lib/types";

function localMidnightS(d: Date): number {
  const m = new Date(d.getFullYear(), d.getMonth(), d.getDate());
  return Math.floor(m.getTime() / 1000);
}

export function HistoryView() {
  const params = useSearchParams();
  const [cams, setCams] = React.useState<Camera[]>([]);
  const [cam, setCam] = React.useState(params.get("cam") ?? "");
  const [day, setDay] = React.useState(() => new Date().toISOString().slice(0, 10));
  const [chunks, setChunks] = React.useState<Chunk[]>([]);
  const [events, setEvents] = React.useState<HubEvent[]>([]);
  const [state, setState] = React.useState<HistoryState | "idle">("idle");
  const [detail, setDetail] = React.useState("");
  const [cursorMs, setCursorMs] = React.useState<number | null>(null);
  const [startInput, setStartInput] = React.useState("");

  const videoRef = React.useRef<HTMLVideoElement>(null);
  const sessionRef = React.useRef<HistorySession | null>(null);

  React.useEffect(() => {
    fetchCameras()
      .then((c) => {
        setCams(c);
        if (!cam && c.length) setCam(c[0].id);
      })
      .catch(() => {});
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Load the day's chunks + events whenever cam/day changes.
  React.useEffect(() => {
    if (!cam || !day) return;
    const d0 = new Date(day + "T00:00:00");
    const s0 = localMidnightS(d0);
    Promise.all([fetchChunks(cam, s0, s0 + 86400), fetchEvents(cam, s0, s0 + 86400)])
      .then(([c, e]) => {
        setChunks(c);
        setEvents(e);
      })
      .catch(() => {
        setChunks([]);
        setEvents([]);
      });
  }, [cam, day]);

  React.useEffect(
    () => () => {
      sessionRef.current?.destroy();
    },
    [],
  );

  const play = async (wallMs: number) => {
    if (!cam || !videoRef.current) return;
    sessionRef.current?.destroy();
    sessionRef.current = null;
    setCursorMs(wallMs);
    setState("loading");
    try {
      sessionRef.current = await HistorySession.open(
        cam,
        wallMs,
        videoRef.current,
        (s, d) => {
          setState(s);
          setDetail(d ?? "");
        },
        (ms) => setCursorMs(ms),
      );
    } catch (e) {
      setState("error");
      setDetail(String(e));
    }
  };

  const playFromInput = () => {
    const t = new Date(`${day}T${startInput || "00:00"}:00`).getTime();
    if (Number.isFinite(t)) play(t);
  };

  const dayStartS = localMidnightS(new Date(day + "T00:00:00"));

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-end gap-3">
        <div className="grid gap-1.5">
          <Label htmlFor="cam">Camera</Label>
          <Select value={cam} onValueChange={setCam}>
            <SelectTrigger id="cam" className="w-44">
              <SelectValue placeholder="pick camera" />
            </SelectTrigger>
            <SelectContent>
              {cams.map((c) => (
                <SelectItem key={c.id} value={c.id}>
                  {c.id}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor="day">Day</Label>
          <Input
            id="day"
            type="date"
            value={day}
            onChange={(e) => setDay(e.target.value)}
            className="w-40"
          />
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor="start">Start time</Label>
          <div className="flex gap-2">
            <Input
              id="start"
              type="time"
              step="1"
              value={startInput}
              onChange={(e) => setStartInput(e.target.value)}
              className="w-32"
            />
            <Button onClick={playFromInput} disabled={!cam}>
              Play
            </Button>
          </div>
        </div>
      </div>

      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="text-sm font-medium">
            {day} — {chunks.length} chunks
            {events.length > 0 && (
              <span className="ml-2 text-red-500">{events.length} events</span>
            )}
          </CardTitle>
        </CardHeader>
        <CardContent>
          <Timeline
            dayStartS={dayStartS}
            chunks={chunks}
            events={events}
            cursorMs={cursorMs}
            onPick={play}
          />
        </CardContent>
      </Card>

      <div className="relative aspect-video w-full overflow-hidden rounded-xl bg-black">
        {/* eslint-disable-next-line jsx-a11y/media-has-caption */}
        <video ref={videoRef} className="h-full w-full" controls playsInline />
        {state !== "playing" && state !== "idle" && (
          <div className="absolute inset-0 flex flex-col items-center justify-center gap-2 bg-black/60 text-sm text-muted-foreground">
            {state === "loading" ? (
              <Loader2 className="size-8 animate-spin" />
            ) : state === "ended" ? (
              <>
                <VideoOff className="size-8" />
                <span>end of recorded footage</span>
              </>
            ) : (
              <>
                <VideoOff className="size-8" />
                <span>{detail || "playback error"}</span>
              </>
            )}
          </div>
        )}
        {state === "idle" && (
          <div className="absolute inset-0 flex items-center justify-center text-sm text-muted-foreground">
            pick a time on the strip — or type one above — to start playback
          </div>
        )}
      </div>
    </div>
  );
}
