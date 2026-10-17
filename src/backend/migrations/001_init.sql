-- surveillance-os catalog schema (plans/backend.md §2.1)
-- Apply: psql "$DATABASE_URL" -f migrations/001_init.sql

create table if not exists cameras (
  id           text primary key,          -- 'home-a', human-set slug
  site         text not null,
  tailnet_name text not null,             -- expected whois node name
  status       text not null default 'offline',
  last_seen    timestamptz,
  fw_version   text,
  created_at   timestamptz not null default now()
);

create table if not exists chunks (
  cam_id       text not null references cameras(id),
  window_start timestamptz not null,
  window_end   timestamptz not null,
  r2_key       text not null,
  manifest_key text not null,
  bytes        bigint not null,
  duration_s   real not null,
  sha256       text not null,
  dropped_s    real not null default 0,
  suspect      boolean not null default false,
  created_at   timestamptz not null default now(),
  primary key (cam_id, window_start)      -- makes the chunker idempotent
);

create table if not exists events (
  id     bigserial primary key,
  cam_id text not null references cameras(id),
  ts     timestamptz not null default now(),
  kind   text not null,                   -- gap|silence|suspect_clock|offline|online
  meta   jsonb
);

create table if not exists gaps (
  cam_id   text not null references cameras(id),
  ts_start timestamptz not null,
  ts_end   timestamptz,
  reason   text,                          -- spool_overflow|offline|suspect_clock
  primary key (cam_id, ts_start)
);
