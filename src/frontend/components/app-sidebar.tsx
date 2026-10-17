"use client";

import * as React from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import {
  Camera,
  History,
  LayoutGrid,
  Video,
  ShieldCheck,
} from "lucide-react";

import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuBadge,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarRail,
} from "@/components/ui/sidebar";
import { Badge } from "@/components/ui/badge";
import { fetchCameras, fetchStatus } from "@/lib/api";
import type { Camera as Cam, HubStatus } from "@/lib/types";

export function AppSidebar() {
  const pathname = usePathname();
  const [cams, setCams] = React.useState<Cam[]>([]);
  const [status, setStatus] = React.useState<HubStatus | null>(null);

  React.useEffect(() => {
    let dead = false;
    const poll = async () => {
      try {
        const [c, s] = await Promise.all([fetchCameras(), fetchStatus()]);
        if (!dead) {
          setCams(c);
          setStatus(s);
        }
      } catch {
        /* hub unreachable — sidebar just stays empty */
      }
    };
    poll();
    const t = setInterval(poll, 5000);
    return () => {
      dead = true;
      clearInterval(t);
    };
  }, []);

  const online = cams.filter((c) => c.status === "online").length;

  return (
    <Sidebar collapsible="icon">
      <SidebarHeader>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton size="lg" asChild>
              <Link href="/live">
                <div className="bg-primary text-primary-foreground flex aspect-square size-8 items-center justify-center rounded-lg">
                  <ShieldCheck className="size-4" />
                </div>
                <div className="grid flex-1 text-left text-sm leading-tight">
                  <span className="truncate font-semibold">Surveillance</span>
                  <span className="truncate text-xs text-muted-foreground">
                    {online}/{cams.length} cameras live
                  </span>
                </div>
              </Link>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>

      <SidebarContent>
        <SidebarGroup>
          <SidebarGroupLabel>View</SidebarGroupLabel>
          <SidebarGroupContent>
            <SidebarMenu>
              <SidebarMenuItem>
                <SidebarMenuButton asChild isActive={pathname === "/live"}>
                  <Link href="/live">
                    <LayoutGrid />
                    <span>Live</span>
                  </Link>
                </SidebarMenuButton>
                <SidebarMenuBadge>{online}</SidebarMenuBadge>
              </SidebarMenuItem>
              <SidebarMenuItem>
                <SidebarMenuButton asChild isActive={pathname.startsWith("/history")}>
                  <Link href="/history">
                    <History />
                    <span>History</span>
                  </Link>
                </SidebarMenuButton>
              </SidebarMenuItem>
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>

        <SidebarGroup>
          <SidebarGroupLabel>Cameras</SidebarGroupLabel>
          <SidebarGroupContent>
            <SidebarMenu>
              {cams.length === 0 && (
                <SidebarMenuItem>
                  <SidebarMenuButton disabled>
                    <Camera className="opacity-40" />
                    <span className="text-muted-foreground">none found</span>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              )}
              {cams.map((c) => (
                <SidebarMenuItem key={c.id}>
                  <SidebarMenuButton
                    asChild
                    isActive={pathname === `/live/${c.id}`}
                  >
                    <Link href={`/live/${c.id}`}>
                      <Video
                        className={
                          c.status === "online"
                            ? "text-emerald-500"
                            : "text-muted-foreground"
                        }
                      />
                      <span>{c.id}</span>
                    </Link>
                  </SidebarMenuButton>
                  <SidebarMenuBadge>
                    <Badge
                      variant={c.status === "online" ? "default" : "secondary"}
                      className={
                        c.status === "online"
                          ? "bg-emerald-600/15 text-emerald-600 border-emerald-600/30 px-1.5"
                          : "px-1.5"
                      }
                    >
                      {c.status === "online" ? "live" : "off"}
                    </Badge>
                  </SidebarMenuBadge>
                </SidebarMenuItem>
              ))}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>
      </SidebarContent>

      <SidebarFooter>
        <div className="px-2 py-1 text-[11px] text-muted-foreground group-data-[collapsible=icon]:hidden">
          {status ? (
            <>
              hub up {Math.floor(status.uptime_s / 60)}m ·{" "}
              {status.cameras.reduce((a, c) => a + c.ring_subs, 0)} viewers ·{" "}
              {status.mem_alloc_mb}MB
            </>
          ) : (
            "hub unreachable"
          )}
        </div>
      </SidebarFooter>
      <SidebarRail />
    </Sidebar>
  );
}
