import Link from "next/link";
import { Button } from "@/components/ui/button";
import { LiveVideo } from "@/components/live-video";
import { History } from "lucide-react";

export default async function CameraLivePage({
  params,
}: {
  params: Promise<{ cam: string }>;
}) {
  const { cam } = await params;
  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center justify-between">
        <p className="text-sm text-muted-foreground">
          Live stream from <span className="font-medium text-foreground">{cam}</span>
        </p>
        <Button asChild variant="outline" size="sm">
          <Link href={`/history?cam=${encodeURIComponent(cam)}`}>
            <History className="mr-1 size-4" /> History
          </Link>
        </Button>
      </div>
      <LiveVideo camId={cam} />
    </div>
  );
}
