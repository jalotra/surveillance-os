import { StatusDashboard } from "@/components/status-dashboard";
import { VideoPlayer } from "@/components/video-player";
import { ExampleWrapper } from "@/components/example";

export default function Page() {
  return (
    <ExampleWrapper>
      <div className="flex flex-col gap-8 p-8 max-w-7xl mx-auto">
        <div>
          <h1 className="text-3xl font-bold tracking-tight">Surveillance Admin</h1>
          <p className="text-muted-foreground mt-2">Monitor your Raspberry Pi nodes in real-time.</p>
        </div>
        
        <StatusDashboard />
        
        <div className="grid grid-cols-1 lg:grid-cols-2 gap-8">
            <VideoPlayer />
            {/* Placeholder for future features like Timeline or Event Log */}
            <div className="rounded-xl border bg-card text-card-foreground shadow-sm flex items-center justify-center min-h-[400px]">
                <p className="text-muted-foreground">Event Timeline (Coming Soon)</p>
            </div>
        </div>
      </div>
    </ExampleWrapper>
  );
}
