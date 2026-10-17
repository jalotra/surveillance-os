import { LiveGrid } from "@/components/live-grid";

export default function LivePage() {
  return (
    <div className="flex flex-col gap-4">
      <p className="text-sm text-muted-foreground">
        All cameras — thumbnails refresh every 2s. Click a card for the live
        stream.
      </p>
      <LiveGrid />
    </div>
  );
}
