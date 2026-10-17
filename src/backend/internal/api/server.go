// Package api serves the REST surface from plans/backend.md §1.3 plus
// /status and /healthz. Everything sits behind Cloudflare Tunnel +
// Access — no auth code here by design.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/sjalotra/surveillance-os/backend/internal/blob"
	"github.com/sjalotra/surveillance-os/backend/internal/ingest"
	"github.com/sjalotra/surveillance-os/backend/internal/store"
)

// PresignTTL is the playback URL lifetime (plan: 5 minutes).
const PresignTTL = 5 * time.Minute

// Server holds the REST dependencies.
type Server struct {
	Reg     *ingest.Registry
	Store   store.Store
	Blob    blob.Client
	prefix  string // R2 key prefix (fallback when no catalog row exists)
	scratch string // chunker scratch dir — enables dev object serving
	started time.Time
}

func New(reg *ingest.Registry, st store.Store, b blob.Client, r2Prefix, scratch string) *Server {
	if r2Prefix == "" {
		r2Prefix = "surveillance"
	}
	return &Server{Reg: reg, Store: st, Blob: b, prefix: r2Prefix, scratch: scratch, started: time.Now()}
}

// Handler returns the routed handler. Patterns use Go 1.22 method+wildcard routing.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/cameras", s.cameras)
	mux.HandleFunc("GET /api/chunks", s.chunks)
	mux.HandleFunc("GET /api/chunks/{cam}/{window}/urls", s.chunkURLs)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("GET /api/thumb/{cam}", s.thumb)
	mux.HandleFunc("GET /api/dev/object/{cam}/{window}/{kind}", s.devObject)
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "OK")
	})
	return mux
}

// ---- handlers ----

// cameras: GET /api/cameras → [{id, site, status, last_seen, temp_c,
// spool_bytes, thumb_url}]
func (s *Server) cameras(w http.ResponseWriter, r *http.Request) {
	type out struct {
		ID         string    `json:"id"`
		Site       string    `json:"site"`
		Status     string    `json:"status"`
		LastSeen   time.Time `json:"last_seen"`
		TempC      float64   `json:"temp_c"`
		SpoolBytes int64     `json:"spool_bytes"`
		ThumbURL   string    `json:"thumb_url"`
	}
	var list []out
	for _, c := range s.Reg.All() {
		snap := c.Snap()
		status := "offline"
		if snap.Online {
			status = "online"
		}
		o := out{
			ID: snap.ID, Site: snap.Site, Status: status,
			LastSeen: snap.LastFragAt, ThumbURL: "/api/thumb/" + snap.ID,
		}
		if snap.HaveStat {
			o.TempC = snap.Stat.TempC
			o.SpoolBytes = snap.Stat.SpoolBytes
		}
		list = append(list, o)
	}
	writeJSON(w, list)
}

// chunkRow is the /api/chunks response element.
type chunkRow struct {
	WindowStart int64   `json:"window_start"`
	WindowEnd   int64   `json:"window_end"`
	Bytes       int64   `json:"bytes"`
	DurationS   float64 `json:"duration_s"`
	DroppedS    float64 `json:"dropped_s"`
	Suspect     bool    `json:"suspect"`
}

// chunks: GET /api/chunks?cam=&from=&to= (unix seconds) →
// [{window_start, window_end, bytes, duration_s, dropped_s, suspect}]
func (s *Server) chunks(w http.ResponseWriter, r *http.Request) {
	cam := r.URL.Query().Get("cam")
	from, to, err := windowRange(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rows, err := s.Store.ListChunks(r.Context(), cam, from, to)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	list := []chunkRow{}
	for _, c := range rows {
		list = append(list, chunkRow{
			WindowStart: c.WindowStart.Unix(), WindowEnd: c.WindowEnd.Unix(),
			Bytes: c.Bytes, DurationS: c.DurationS, DroppedS: c.DroppedS,
			Suspect: c.Suspect,
		})
	}
	// Dev fallback: no catalog store → synthesize rows from scratch sidecars
	// so the timeline works without Postgres.
	if len(list) == 0 && s.scratch != "" && cam != "" {
		list = s.scratchChunks(cam, from, to)
	}
	writeJSON(w, list)
}

// scratchChunks scans scratch/{cam}/*.json sidecars and synthesizes the
// /api/chunks row shape — dev only, used when the store returned nothing.
func (s *Server) scratchChunks(cam string, from, to time.Time) []chunkRow {
	out := []chunkRow{}
	if strings.ContainsAny(cam, "/\\") {
		return out
	}
	ents, err := os.ReadDir(filepath.Join(s.scratch, cam))
	if err != nil {
		return out
	}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		idx, err := strconv.ParseInt(strings.TrimSuffix(name, ".json"), 10, 64)
		if err != nil {
			continue
		}
		var man struct {
			LastEndMs int64   `json:"last_end_ms"`
			DroppedS  float64 `json:"dropped_s"`
			Suspect   bool    `json:"suspect"`
		}
		data, err := os.ReadFile(filepath.Join(s.scratch, cam, name))
		if err != nil || json.Unmarshal(data, &man) != nil {
			continue
		}
		ws := idx * 600
		we := ws + 600
		if man.LastEndMs > 0 {
			we = man.LastEndMs / 1000
		}
		start := time.Unix(ws, 0)
		if start.Before(from) || !start.Before(to) {
			continue
		}
		var size int64
		if st, err := os.Stat(filepath.Join(s.scratch, cam, name[:len(name)-5]+".frags")); err == nil {
			size = st.Size()
		}
		out = append(out, chunkRow{
			WindowStart: ws, WindowEnd: we, Bytes: size,
			DurationS: float64(we - ws), DroppedS: man.DroppedS, Suspect: man.Suspect,
		})
	}
	return out
}

// chunkURLs: GET /api/chunks/{cam}/{window}/urls →
// {manifest_url, mp4_url} presigned R2 GETs, 5 min TTL.
func (s *Server) chunkURLs(w http.ResponseWriter, r *http.Request) {
	cam := r.PathValue("cam")
	ws, err := strconv.ParseInt(r.PathValue("window"), 10, 64)
	if err != nil {
		http.Error(w, "bad window", http.StatusBadRequest)
		return
	}
	mp4Key := fmt.Sprintf("%s/%s/%d.mp4", s.prefix, cam, ws)
	manKey := fmt.Sprintf("%s/%s/%d.json", s.prefix, cam, ws)
	// Prefer the recorded keys when a catalog row exists.
	if row, err := s.Store.GetChunk(r.Context(), cam, time.Unix(ws, 0).UTC()); err == nil && row != nil {
		mp4Key, manKey = row.R2Key, row.ManifestKey
	}
	mp4URL, err := s.Blob.PresignGet(r.Context(), mp4Key, PresignTTL)
	if err != nil {
		// Dev fallback: no blob store (R2 creds unset) — serve the chunker's
		// scratch files over same-origin URLs so history playback still works.
		mp4URL = fmt.Sprintf("/api/dev/object/%s/%d/mp4", cam, ws)
	}
	manURL, err := s.Blob.PresignGet(r.Context(), manKey, PresignTTL)
	if err != nil {
		manURL = fmt.Sprintf("/api/dev/object/%s/%d/json", cam, ws)
	}
	writeJSON(w, map[string]string{"manifest_url": manURL, "mp4_url": mp4URL})
}

// devObject serves a chunk window from the chunker scratch dir — dev only
// (history playback without R2). {window} is the unix-seconds key; {kind}
// is "mp4" (init+frags concatenated, Range-aware) or "json" (sidecar).
// Returns 404 when the window is absent — finalized-then-GC'd scratch is
// exactly what R2 would have held in prod.
func (s *Server) devObject(w http.ResponseWriter, r *http.Request) {
	if s.scratch == "" {
		http.Error(w, "no scratch dir", http.StatusNotFound)
		return
	}
	cam := r.PathValue("cam")
	if strings.ContainsAny(cam, "/\\") {
		http.Error(w, "bad cam", http.StatusBadRequest)
		return
	}
	ws, err := strconv.ParseInt(r.PathValue("window"), 10, 64)
	if err != nil {
		http.Error(w, "bad window", http.StatusBadRequest)
		return
	}
	base := filepath.Join(s.scratch, cam, strconv.FormatInt(ws/600, 10))
	switch r.PathValue("kind") {
	case "json":
		// The sidecar stores frag-relative keyframe offsets; the manifest
		// contract (and R2 copy) is absolute file offsets — add initLen so
		// dev playback seeks to the same bytes prod would.
		data, err := os.ReadFile(base + ".json")
		if err != nil {
			http.Error(w, "window not found", http.StatusNotFound)
			return
		}
		var man map[string]any
		if err := json.Unmarshal(data, &man); err != nil {
			http.Error(w, "bad sidecar", http.StatusInternalServerError)
			return
		}
		var initLen float64
		if st, err := os.Stat(base + ".init"); err == nil {
			initLen = float64(st.Size())
		}
		if kfs, ok := man["keyframes"].([]any); ok {
			for _, k := range kfs {
				if kf, ok := k.(map[string]any); ok {
					if off, ok := kf["off"].(float64); ok {
						kf["off"] = off + initLen
					}
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(man)
	case "mp4":
		init, err := os.ReadFile(base + ".init")
		if err != nil {
			http.Error(w, "window not found", http.StatusNotFound)
			return
		}
		frags, err := os.ReadFile(base + ".frags")
		if err != nil {
			http.Error(w, "window not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		// ServeContent gives us Range support for keyframe-offset seeks.
		http.ServeContent(w, r, "", time.Now(), bytes.NewReader(append(init, frags...)))
	default:
		http.Error(w, "bad kind", http.StatusBadRequest)
	}
}

// events: GET /api/events?cam=&from=&to=
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	cam := r.URL.Query().Get("cam")
	from, to, err := windowRange(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.Store.ListEvents(r.Context(), cam, from, to, limit)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, rows)
}

// thumb: GET /api/thumb/{cam} — latest JPEG snapshot written by thumb messages.
func (s *Server) thumb(w http.ResponseWriter, r *http.Request) {
	cam := s.Reg.Get(r.PathValue("cam"))
	if cam == nil {
		http.Error(w, "unknown camera", http.StatusNotFound)
		return
	}
	jpeg, at := cam.Thumb()
	if len(jpeg) == 0 {
		http.Error(w, "no thumbnail yet", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Thumb-At", at.UTC().Format(time.RFC3339))
	w.Header().Set("Cache-Control", "no-store")
	w.Write(jpeg)
}

// status: GET /status — hub health for the one dashboard that matters.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	type camStat struct {
		ID           string  `json:"id"`
		Online       bool    `json:"online"`
		LastFragAgeS float64 `json:"last_frag_age_s"`
		RingFrags    int     `json:"ring_frags"`
		RingBytes    int     `json:"ring_bytes"`
		RingSubs     int     `json:"ring_subs"`
		Suspect      bool    `json:"suspect"`
	}
	cams := []camStat{}
	now := time.Now()
	for _, c := range s.Reg.All() {
		snap := c.Snap()
		n, b, subs := c.Ring.Stats()
		var age float64 = -1
		if !snap.LastFragAt.IsZero() {
			age = now.Sub(snap.LastFragAt).Seconds()
		}
		cams = append(cams, camStat{
			ID: snap.ID, Online: snap.Online, LastFragAgeS: age,
			RingFrags: n, RingBytes: b, RingSubs: subs, Suspect: snap.Suspect,
		})
	}
	writeJSON(w, map[string]any{
		"uptime_s":     int64(now.Sub(s.started).Seconds()),
		"goroutines":   runtime.NumGoroutine(),
		"mem_alloc_mb": m.Alloc >> 20,
		"mem_sys_mb":   m.Sys >> 20,
		"num_cpu":      runtime.NumCPU(),
		"go_version":   runtime.Version(),
		"cameras":      cams,
		"time":         now.UTC(),
	})
}

// ---- helpers ----

// windowRange reads ?from=&to= unix-second params, defaulting to the last
// 24 h when absent.
func windowRange(r *http.Request) (from, to time.Time, err error) {
	q := r.URL.Query()
	to = time.Now()
	from = to.Add(-24 * time.Hour)
	if v := q.Get("from"); v != "" {
		sec, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return from, to, fmt.Errorf("bad 'from' (want unix seconds)")
		}
		from = time.Unix(sec, 0)
	}
	if v := q.Get("to"); v != "" {
		sec, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return from, to, fmt.Errorf("bad 'to' (want unix seconds)")
		}
		to = time.Unix(sec, 0)
	}
	return from.UTC(), to.UTC(), nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
