"use client"

import * as React from "react"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { VideoIcon, VideoOffIcon, WifiIcon, WifiOffIcon } from "lucide-react"

export function VideoPlayer() {
  const [isConnected, setIsConnected] = React.useState(false)
  const [frame, setFrame] = React.useState<string | null>(null)
  const wsRef = React.useRef<WebSocket | null>(null)

  React.useEffect(() => {
    // In production, this URL should be dynamic based on the camera/tenant
    const ws = new WebSocket("ws://localhost:8081/stream")

    ws.onopen = () => {
      console.log("WebSocket connected")
      setIsConnected(true)
    }

    ws.onclose = () => {
      console.log("WebSocket disconnected")
      setIsConnected(false)
    }

    ws.onmessage = (event) => {
      // Assuming server sends raw base64 encoded MJPEG frame
      setFrame(`data:image/jpeg;base64,${event.data}`)
    }

    wsRef.current = ws

    return () => {
      ws.close()
    }
  }, [])

  return (
    <Card className="col-span-1 lg:col-span-2">
      <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
        <div className="flex flex-col gap-1">
          <CardTitle>Live Feed</CardTitle>
          <CardDescription>
            {isConnected ? "Streaming via WebSocket" : "Connecting..."}
          </CardDescription>
        </div>
        <Badge variant={isConnected ? "default" : "destructive"}>
          {isConnected ? (
            <WifiIcon className="mr-1 h-3 w-3" />
          ) : (
            <WifiOffIcon className="mr-1 h-3 w-3" />
          )}
          {isConnected ? "Live" : "Offline"}
        </Badge>
      </CardHeader>
      <CardContent>
        <div className="aspect-video w-full overflow-hidden rounded-lg bg-black/90 relative flex items-center justify-center border border-border">
          {frame ? (
            <img
              src={frame}
              alt="Live Stream"
              className="h-full w-full object-contain"
            />
          ) : (
            <div className="flex flex-col items-center gap-2 text-muted-foreground">
              <VideoOffIcon className="h-10 w-10 opacity-50" />
              <p>Waiting for stream...</p>
            </div>
          )}
        </div>
      </CardContent>
    </Card>
  )
}
