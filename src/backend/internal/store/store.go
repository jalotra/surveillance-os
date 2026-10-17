// Package store is the Postgres catalog (PlanetScale). pgx pool behind the
// Store interface; a Null implementation lets the hub run without a DSN in
// dev. Schema lives in migrations/001_init.sql and matches plans/backend.md
// §2.1 exactly.
//
// Durability rule: Postgres is never the source of truth — R2 is. Every
// write here is retried by callers and rebuild-catalog can reconstruct all
// rows from the bucket.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Camera is a row of the cameras table.
type Camera struct {
	ID          string
	Site        string
	TailnetName string
	Status      string
	LastSeen    *time.Time
	FWVersion   string
}

// Chunk is a row of the chunks table.
type Chunk struct {
	CamID       string
	WindowStart time.Time
	WindowEnd   time.Time
	R2Key       string
	ManifestKey string
	Bytes       int64
	DurationS   float64
	SHA256      string
	DroppedS    float64
	Suspect     bool
}

// Event is a row of the events table. JSON tags keep /api/events
// snake_case like the rest of the API surface.
type Event struct {
	ID    int64           `json:"id"`
	CamID string          `json:"cam_id"`
	TS    time.Time       `json:"ts"`
	Kind  string          `json:"kind"`
	Meta  json.RawMessage `json:"meta"`
}

// Event kinds (events.kind).
const (
	EventGap          = "gap"
	EventSilence      = "silence"
	EventSuspectClock = "suspect_clock"
	EventOffline      = "offline"
	EventOnline       = "online"
)

// Store is the catalog surface used by the rest of the hub.
type Store interface {
	UpsertCamera(ctx context.Context, c Camera) error
	EnsureCamera(ctx context.Context, c Camera) error // insert-if-missing, never overwrites
	ListCameras(ctx context.Context) ([]Camera, error)
	SetCameraStatus(ctx context.Context, id, status string, online bool, fw string) error
	UpsertChunk(ctx context.Context, c Chunk) error
	GetChunk(ctx context.Context, camID string, windowStart time.Time) (*Chunk, error)
	ListChunks(ctx context.Context, camID string, from, to time.Time) ([]Chunk, error)
	InsertEvent(ctx context.Context, camID, kind string, meta any) error
	ListEvents(ctx context.Context, camID string, from, to time.Time, limit int) ([]Event, error)
	InsertGap(ctx context.Context, camID string, tsStart time.Time, tsEnd *time.Time, reason string) error
	Close()
}

// DB is the pgx-backed Store.
type DB struct {
	pool *pgxpool.Pool
}

// Connect creates a pool. It does not verify connectivity — call Ping.
func Connect(ctx context.Context, dsn string) (*DB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Ping verifies the pool.
func (d *DB) Ping(ctx context.Context) error { return d.pool.Ping(ctx) }

func (d *DB) Close() { d.pool.Close() }

func (d *DB) UpsertCamera(ctx context.Context, c Camera) error {
	_, err := d.pool.Exec(ctx, `
		insert into cameras (id, site, tailnet_name) values ($1, $2, $3)
		on conflict (id) do update set site = excluded.site,
		  tailnet_name = excluded.tailnet_name`,
		c.ID, c.Site, c.TailnetName)
	return err
}

func (d *DB) EnsureCamera(ctx context.Context, c Camera) error {
	_, err := d.pool.Exec(ctx, `
		insert into cameras (id, site, tailnet_name) values ($1, $2, $3)
		on conflict (id) do nothing`,
		c.ID, c.Site, c.TailnetName)
	return err
}

func (d *DB) ListCameras(ctx context.Context) ([]Camera, error) {
	rows, err := d.pool.Query(ctx,
		`select id, site, tailnet_name, status, last_seen, coalesce(fw_version,'') from cameras order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Camera
	for rows.Next() {
		var c Camera
		if err := rows.Scan(&c.ID, &c.Site, &c.TailnetName, &c.Status, &c.LastSeen, &c.FWVersion); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetCameraStatus marks a camera online/offline. Online also bumps
// last_seen and refreshes fw_version.
func (d *DB) SetCameraStatus(ctx context.Context, id, status string, online bool, fw string) error {
	var err error
	if online {
		_, err = d.pool.Exec(ctx, `
			update cameras set status = $2, last_seen = now(),
			  fw_version = nullif($3, '') where id = $1`, id, status, fw)
	} else {
		_, err = d.pool.Exec(ctx,
			`update cameras set status = $2 where id = $1`, id, status)
	}
	return err
}

func (d *DB) UpsertChunk(ctx context.Context, c Chunk) error {
	_, err := d.pool.Exec(ctx, `
		insert into chunks (cam_id, window_start, window_end, r2_key, manifest_key,
		                    bytes, duration_s, sha256, dropped_s, suspect)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		on conflict (cam_id, window_start) do update set
		  window_end   = excluded.window_end,
		  r2_key       = excluded.r2_key,
		  manifest_key = excluded.manifest_key,
		  bytes        = excluded.bytes,
		  duration_s   = excluded.duration_s,
		  sha256       = excluded.sha256,
		  dropped_s    = excluded.dropped_s,
		  suspect      = excluded.suspect`,
		c.CamID, c.WindowStart, c.WindowEnd, c.R2Key, c.ManifestKey,
		c.Bytes, float32(c.DurationS), c.SHA256, float32(c.DroppedS), c.Suspect)
	return err
}

func (d *DB) GetChunk(ctx context.Context, camID string, windowStart time.Time) (*Chunk, error) {
	var c Chunk
	var dur, dropped float32 // real columns
	err := d.pool.QueryRow(ctx, `
		select cam_id, window_start, window_end, r2_key, manifest_key,
		       bytes, duration_s, sha256, dropped_s, suspect
		from chunks where cam_id = $1 and window_start = $2`, camID, windowStart).
		Scan(&c.CamID, &c.WindowStart, &c.WindowEnd, &c.R2Key, &c.ManifestKey,
			&c.Bytes, &dur, &c.SHA256, &dropped, &c.Suspect)
	c.DurationS, c.DroppedS = float64(dur), float64(dropped)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (d *DB) ListChunks(ctx context.Context, camID string, from, to time.Time) ([]Chunk, error) {
	rows, err := d.pool.Query(ctx, `
		select cam_id, window_start, window_end, r2_key, manifest_key,
		       bytes, duration_s, sha256, dropped_s, suspect
		from chunks
		where cam_id = $1 and window_start >= $2 and window_start <= $3
		order by window_start`, camID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chunk
	for rows.Next() {
		var c Chunk
		var dur, dropped float32
		if err := rows.Scan(&c.CamID, &c.WindowStart, &c.WindowEnd, &c.R2Key,
			&c.ManifestKey, &c.Bytes, &dur, &c.SHA256, &dropped, &c.Suspect); err != nil {
			return nil, err
		}
		c.DurationS, c.DroppedS = float64(dur), float64(dropped)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) InsertEvent(ctx context.Context, camID, kind string, meta any) error {
	var m []byte
	if meta != nil {
		var err error
		if m, err = json.Marshal(meta); err != nil {
			return fmt.Errorf("event meta: %w", err)
		}
	}
	_, err := d.pool.Exec(ctx,
		`insert into events (cam_id, kind, meta) values ($1, $2, $3)`, camID, kind, m)
	return err
}

func (d *DB) ListEvents(ctx context.Context, camID string, from, to time.Time, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 200
	}
	q := `select id, cam_id, ts, kind, coalesce(meta,'{}'::jsonb) from events
	      where ts >= $1 and ts <= $2`
	args := []any{from, to}
	if camID != "" {
		q += ` and cam_id = $3`
		args = append(args, camID)
	}
	q += ` order by ts desc limit ` + fmt.Sprint(limit)
	rows, err := d.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.CamID, &e.TS, &e.Kind, &e.Meta); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (d *DB) InsertGap(ctx context.Context, camID string, tsStart time.Time, tsEnd *time.Time, reason string) error {
	_, err := d.pool.Exec(ctx, `
		insert into gaps (cam_id, ts_start, ts_end, reason) values ($1, $2, $3, $4)
		on conflict (cam_id, ts_start) do update set
		  ts_end = excluded.ts_end, reason = excluded.reason`,
		camID, tsStart, tsEnd, reason)
	return err
}

// Null is a no-op Store for dev when DATABASE_URL is unset. Writes are
// logged-and-dropped; reads return empty results. Chunks still land in the
// blob store — rebuild-catalog backfills the rows later.
type Null struct{}

func (Null) UpsertCamera(context.Context, Camera) error    { return nil }
func (Null) EnsureCamera(context.Context, Camera) error    { return nil }
func (Null) ListCameras(context.Context) ([]Camera, error) { return nil, nil }
func (Null) SetCameraStatus(context.Context, string, string, bool, string) error {
	return nil
}
func (Null) UpsertChunk(context.Context, Chunk) error                    { return nil }
func (Null) GetChunk(context.Context, string, time.Time) (*Chunk, error) { return nil, nil }
func (Null) ListChunks(context.Context, string, time.Time, time.Time) ([]Chunk, error) {
	return nil, nil
}
func (Null) InsertEvent(context.Context, string, string, any) error { return nil }
func (Null) ListEvents(context.Context, string, time.Time, time.Time, int) ([]Event, error) {
	return nil, nil
}
func (Null) InsertGap(context.Context, string, time.Time, *time.Time, string) error {
	return nil
}
func (Null) Close() {}
