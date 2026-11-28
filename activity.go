package main

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const activityLimit = 1000

type event struct {
	At     time.Time `json:"at"`
	Name   string    `json:"name"`
	Kind   string    `json:"kind,omitempty"` // "user" for account changes; otherwise a database
	Action string    `json:"action"`
	Result string    `json:"result"`
	Detail string    `json:"detail,omitempty"`
	Actor  string    `json:"actor,omitempty"` // account, "admin token" or "system"
	// Set when the event's alert was already sent, as for health changes.
	alerted bool
}

// The event history, newest first. The console is the only writer of
// activity.json, so it is read once and then kept in memory; each event
// replaces the list, so a list once returned never changes.
func (m *manager) eventLog() ([]event, error) {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()
	return m.loadEvents()
}

func (m *manager) loadEvents() ([]event, error) {
	if m.eventsRead {
		return m.events, nil
	}
	items := []event{}
	err := readJSON(filepath.Join(m.dir, "activity.json"), &items)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	m.events, m.eventsRead = items, true
	return items, nil
}

// A copy of the history, for callers that change it.
func (m *manager) readActivity() ([]event, error) {
	items, err := m.eventLog()
	return slices.Clone(items), err
}

// Record an event. A failure is also sent to the notification channels.
func (m *manager) logEvent(ctx context.Context, e event) {
	e.At, e.Actor = time.Now().UTC(), actorFrom(ctx)
	m.activityMu.Lock()
	items, err := m.loadEvents()
	if err == nil {
		next := make([]event, 0, min(len(items)+1, activityLimit))
		next = append(next, e)
		next = append(next, items[:min(len(items), activityLimit-1)]...)
		m.events = next
		err = writeJSON(filepath.Join(m.dir, "activity.json"), next)
	}
	m.activityMu.Unlock()
	if err != nil {
		log.Printf("writing activity history: %v", err)
	}
	if e.Result == "failed" && !e.alerted {
		m.alertFailure(e)
	}
}

func (m *manager) note(ctx context.Context, name, action, result, detail string) {
	m.logEvent(ctx, event{Name: name, Action: action, Result: result, Detail: detail})
}

// Account changes are recorded under the username they affect.
func (m *manager) noteUser(ctx context.Context, username, action, detail string) {
	m.logEvent(ctx, event{Name: username, Kind: "user", Action: action, Result: "completed", Detail: detail})
}

// Only operator-facing problem texts are recorded. Other errors can carry
// internal detail and stay in the control-plane log, as in API responses.
func describe(err error) string {
	var p *problem
	if !errors.As(err, &p) {
		return "Check the control-plane logs for details."
	}
	if text := []rune(p.text); len(text) > 600 {
		return string(text[:600]) + "…"
	}
	return p.text
}

func (m *manager) track(ctx context.Context, name, action string) func(error) {
	return m.trackDetail(ctx, name, action, "")
}

// Like track, with a detail such as the database user an action affects.
func (m *manager) trackDetail(ctx context.Context, name, action, detail string) func(error) {
	m.note(ctx, name, action, "started", detail)
	return func(err error) {
		if err != nil {
			m.note(ctx, name, action, "failed", describe(err))
			return
		}
		m.note(ctx, name, action, "completed", detail)
	}
}
