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
import { ActivityIcon, CpuIcon, DatabaseIcon, HardDriveIcon, ClockIcon } from "lucide-react"

interface SystemStatus {
  status: string
  uptime: string
  go_version: string
  num_cpu: number
  goroutines: number
  memory_mb: number
  timestamp: string
}

export function StatusDashboard() {
  const [status, setStatus] = React.useState<SystemStatus | null>(null)
  const [error, setError] = React.useState<string | null>(null)
  const [lastHeartbeat, setLastHeartbeat] = React.useState<Date | null>(null)

  const fetchStatus = React.useCallback(async () => {
    try {
      // Assuming the Raspberry Pi is at localhost:8080 during local testing
      // In production, this would be the Pi's IP address
      const res = await fetch("http://localhost:8080/status")
      if (!res.ok) throw new Error("Failed to fetch status")
      const data: SystemStatus = await res.json()
      setStatus(data)
      setLastHeartbeat(new Date())
      setError(null)
    } catch (err) {
      setError("Disconnected from Raspberry Pi")
      setStatus(prev => prev ? { ...prev, status: "offline" } : null)
    }
  }, [])

  React.useEffect(() => {
    fetchStatus()
    const interval = setInterval(fetchStatus, 2000) // Heartbeat every 2 seconds
    return () => clearInterval(interval)
  }, [fetchStatus])

  return (
    <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
      <Card>
        <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
          <CardTitle>System Status</CardTitle>
          <ActivityIcon className="text-muted-foreground h-4 w-4" />
        </CardHeader>
        <CardContent>
          <div className="flex items-center gap-2">
            <div className={`h-2 w-2 rounded-full ${status?.status === "online" ? "bg-green-500 animate-pulse" : "bg-red-500"}`} />
            <div className="text-2xl font-bold capitalize leading-none">{status?.status || "Unknown"}</div>
          </div>
          <CardDescription className="mt-2">
            {error || `Last heartbeat: ${lastHeartbeat?.toLocaleTimeString()}`}
          </CardDescription>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
          <CardTitle>CPU Usage</CardTitle>
          <CpuIcon className="text-muted-foreground h-4 w-4" />
        </CardHeader>
        <CardContent>
          <div className="text-2xl font-bold leading-none">{status?.num_cpu || 0} Cores</div>
          <CardDescription className="mt-2">
            {status?.goroutines || 0} active goroutines
          </CardDescription>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
          <CardTitle>Memory</CardTitle>
          <DatabaseIcon className="text-muted-foreground h-4 w-4" />
        </CardHeader>
        <CardContent>
          <div className="text-2xl font-bold leading-none">{status?.memory_mb || 0} MB</div>
          <CardDescription className="mt-2">
            Go Version: {status?.go_version || "N/A"}
          </CardDescription>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
          <CardTitle>Uptime</CardTitle>
          <ClockIcon className="text-muted-foreground h-4 w-4" />
        </CardHeader>
        <CardContent>
          <div className="text-2xl font-bold leading-none truncate">{status?.uptime || "0s"}</div>
          <CardDescription className="mt-2">
            Since system start
          </CardDescription>
        </CardContent>
      </Card>
    </div>
  )
}
