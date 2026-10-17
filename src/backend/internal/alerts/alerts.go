// Package alerts is the whole monitoring stack: every 30s it checks each
// camera's last-fragment age and throttled flag, and pings Telegram when
// a camera has been silent >2min or reports a non-zero throttle state.
// "Last fragment age > 2 min" is deliberately the only critical alert —
// it covers freezes, dead Pis and tailnet drops alike.
package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/sjalotra/surveillance-os/backend/internal/ingest"
	"github.com/sjalotra/surveillance-os/backend/internal/store"
)

const (
	// Interval is the check cadence.
	Interval = 30 * time.Second
	// SilenceAfter is the silence threshold (failure table: 2 min).
	SilenceAfter = 2 * time.Minute
	// RemindEvery re-sends a continuing alert at this cadence.
	RemindEvery = 30 * time.Minute
)

// Sender delivers one alert text.
type Sender interface {
	Send(ctx context.Context, text string) error
}

// Telegram sends via the Bot API sendMessage endpoint.
type Telegram struct {
	Token  string
	ChatID string
	Client *http.Client // optional; default http.DefaultClient
}

func (t Telegram) Send(ctx context.Context, text string) error {
	if t.Token == "" || t.ChatID == "" {
		return fmt.Errorf("telegram not configured")
	}
	body, _ := json.Marshal(map[string]string{"chat_id": t.ChatID, "text": text})
	cli := t.Client
	if cli == nil {
		cli = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.telegram.org/bot"+t.Token+"/sendMessage",
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cli.Do(req)
	if err != nil {
		return fmt.Errorf("telegram send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram send: status %d", resp.StatusCode)
	}
	return nil
}

// LogSender is a Sender that only logs — used when Telegram isn't set up.
type LogSender struct{}

func (LogSender) Send(_ context.Context, text string) error {
	log.Printf("ALERT (telegram not configured): %s", text)
	return nil
}

type camAlert struct {
	silentSince time.Time // zero → not currently alerting
	throttled   bool
	lastSent    time.Time
}

// Alerter runs the periodic check.
type Alerter struct {
	Reg    *ingest.Registry
	Sender Sender
	Store  store.Store
	Logf   func(string, ...any)

	mu     sync.Mutex
	alarms map[string]*camAlert
}

func New(reg *ingest.Registry, sender Sender, st store.Store) *Alerter {
	if sender == nil {
		sender = LogSender{}
	}
	return &Alerter{Reg: reg, Sender: sender, Store: st, alarms: map[string]*camAlert{}}
}

func (a *Alerter) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

// Run checks every Interval until ctx is done.
func (a *Alerter) Run(ctx context.Context) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	a.Check(ctx) // first pass immediately
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.Check(ctx)
		}
	}
}

// Check evaluates all cameras once — also called directly by tests.
func (a *Alerter) Check(ctx context.Context) {
	now := time.Now()
	for _, cam := range a.Reg.All() {
		snap := cam.Snap()
		state := a.state(cam.ID)

		// Silence: stale last-frag when we have one; a camera that has
		// never produced (or is offline) gets a startup grace equal to
		// the threshold so the fleet doesn't alert at hub boot.
		silent := true
		if !snap.LastFragAt.IsZero() {
			silent = now.Sub(snap.LastFragAt) > SilenceAfter
		} else {
			silent = now.Sub(cam.RegisteredAt()) > SilenceAfter
		}
		if silent {
			since := state.silentSince
			if since.IsZero() {
				since = now
				state.silentSince = now
				a.event(ctx, snap.ID, store.EventSilence,
					map[string]any{"last_frag": snap.LastFragAt, "online": snap.Online})
			}
			if now.Sub(state.lastSent) >= RemindEvery || state.lastSent.IsZero() {
				a.send(ctx, fmt.Sprintf("⚠️ camera %s silent: no fragments for >%s (online=%v)",
					snap.ID, SilenceAfter, snap.Online))
				state.lastSent = now
			}
		} else if !state.silentSince.IsZero() {
			// recovered
			down := now.Sub(state.silentSince).Round(time.Second)
			state.silentSince = time.Time{}
			state.lastSent = time.Time{}
			a.send(ctx, fmt.Sprintf("✅ camera %s back after %s", snap.ID, down))
		}

		// Throttle/under-voltage: any non-"0x0" value.
		if snap.HaveStat {
			throttling := snap.Stat.Throttled != "" && snap.Stat.Throttled != "0x0"
			if throttling && !state.throttled {
				state.throttled = true
				a.send(ctx, fmt.Sprintf("⚠️ camera %s throttled=%s (temp %.1f°C) — check PSU",
					snap.ID, snap.Stat.Throttled, snap.Stat.TempC))
			} else if !throttling {
				state.throttled = false
			}
		}
	}
}

func (a *Alerter) state(camID string) *camAlert {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.alarms[camID]
	if st == nil {
		st = &camAlert{}
		a.alarms[camID] = st
	}
	return st
}

func (a *Alerter) send(ctx context.Context, text string) {
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := a.Sender.Send(sctx, text); err != nil {
		a.logf("alerts: send: %v", err)
	}
}

func (a *Alerter) event(ctx context.Context, camID, kind string, meta any) {
	if a.Store == nil {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = a.Store.InsertEvent(sctx, camID, kind, meta)
}
