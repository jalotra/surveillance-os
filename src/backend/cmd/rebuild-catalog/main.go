// rebuild-catalog reconstructs the entire Postgres catalog from R2 —
// the proof that the bucket is the real source of truth. It LISTs the
// bucket under the key prefix, fetches every {window}.json manifest,
// and upserts cameras + chunks (+ gap rows for dropped_s>0). Safe to
// re-run; everything is keyed and idempotent.
//
//	Usage: DATABASE_URL=... R2_ENDPOINT=... R2_BUCKET=... R2_ACCESS_KEY_ID=... \
//	       R2_SECRET_ACCESS_KEY=... go run ./cmd/rebuild-catalog
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sjalotra/surveillance-os/backend/internal/blob"
	"github.com/sjalotra/surveillance-os/backend/internal/chunker"
	"github.com/sjalotra/surveillance-os/backend/internal/config"
	"github.com/sjalotra/surveillance-os/backend/internal/store"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	cfg, err := config.FromEnv()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.DatabaseURL == "" {
		log.Fatal("DATABASE_URL required")
	}
	if !cfg.R2Configured() {
		log.Fatal("R2 credentials required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	db, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		log.Fatalf("postgres: %v", err)
	}
	blobs := blob.NewR2(cfg.R2Endpoint, cfg.R2Bucket, cfg.R2AccessKey, cfg.R2SecretKey)

	// LIST every object under the prefix and pick out manifests.
	objs, err := blobs.List(ctx, cfg.R2Prefix+"/")
	if err != nil {
		log.Fatalf("list: %v", err)
	}
	type manRef struct {
		cam, key string
		window   int64
	}
	var mans []manRef
	for _, o := range objs {
		// Strip the prefix first so multi-segment prefixes still work.
		rel, ok := strings.CutPrefix(o.Key, cfg.R2Prefix+"/")
		if !ok {
			continue
		}
		parts := strings.Split(rel, "/")
		if len(parts) != 2 || !strings.HasSuffix(parts[1], ".json") {
			continue
		}
		w, err := strconv.ParseInt(strings.TrimSuffix(parts[1], ".json"), 10, 64)
		if err != nil {
			continue
		}
		mans = append(mans, manRef{parts[0], o.Key, w})
	}
	sort.Slice(mans, func(i, j int) bool {
		if mans[i].cam != mans[j].cam {
			return mans[i].cam < mans[j].cam
		}
		return mans[i].window < mans[j].window
	})

	log.Printf("found %d manifests under %s/", len(mans), cfg.R2Prefix)
	var chunks, cameras, gaps, errs int
	for _, m := range mans {
		rc, err := blobs.Get(ctx, m.key)
		if err != nil {
			log.Printf("get %s: %v", m.key, err)
			errs++
			continue
		}
		var man chunker.Manifest
		err = json.NewDecoder(rc).Decode(&man)
		rc.Close()
		if err != nil {
			log.Printf("decode %s: %v", m.key, err)
			errs++
			continue
		}
		if man.Cam != "" && man.Cam != m.cam {
			log.Printf("WARNING: %s manifest cam %q != key cam %q", m.key, man.Cam, m.cam)
		}
		cam := m.cam
		if man.Cam != "" {
			cam = man.Cam
		}

		if err := db.EnsureCamera(ctx, store.Camera{
			ID: cam, Site: "restored", TailnetName: "",
		}); err != nil {
			log.Printf("ensure camera %s: %v", cam, err)
			errs++
			continue
		}
		cameras++

		mp4Key := fmt.Sprintf("%s/%s/%d.mp4", cfg.R2Prefix, cam, man.WindowStart)
		if err := db.UpsertChunk(ctx, store.Chunk{
			CamID:       cam,
			WindowStart: time.Unix(man.WindowStart, 0).UTC(),
			WindowEnd:   time.Unix(man.WindowStart+600, 0).UTC(),
			R2Key:       mp4Key,
			ManifestKey: m.key,
			Bytes:       man.Bytes,
			DurationS:   man.DurationS,
			SHA256:      man.SHA256,
			DroppedS:    man.DroppedS,
			Suspect:     man.Suspect,
		}); err != nil {
			log.Printf("upsert chunk %s/%d: %v", cam, man.WindowStart, err)
			errs++
			continue
		}
		chunks++

		if man.DroppedS > 0 {
			start := time.Unix(man.WindowStart, 0).UTC()
			end := time.Unix(man.WindowStart+600, 0).UTC()
			if err := db.InsertGap(ctx, cam, start, &end, "spool_overflow"); err != nil {
				log.Printf("gap %s/%d: %v", cam, man.WindowStart, err)
				errs++
			} else {
				gaps++
			}
		}
	}

	log.Printf("rebuild-catalog done: %d cameras, %d chunks, %d gaps, %d errors",
		cameras, chunks, gaps, errs)
	if errs > 0 {
		os.Exit(1)
	}
}
