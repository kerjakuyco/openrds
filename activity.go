package main

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"
)

const activityLimit = 1000

type event struct {
	At     time.Time `json:"at"`
	Name   string    `json:"name"`
	Action string    `json:"action"`
	Result string    `json:"result"`
	Detail string    `json:"detail,omitempty"`
}

func (m *manager) readActivity() ([]event, error) {
	items := []event{}
	err := readJSON(filepath.Join(m.dir, "activity.json"), &items)
	if errors.Is(err, os.ErrNotExist) {
		return items, nil
	}
	return items, err
}

func (m *manager) note(name, action, result, detail string) {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()
	items, err := m.readActivity()
	if err == nil {
		items = append([]event{{time.Now().UTC(), name, action, result, detail}}, items...)
		if len(items) > activityLimit {
			items = items[:activityLimit]
		}
		err = writeJSON(filepath.Join(m.dir, "activity.json"), items)
	}
	if err != nil {
		log.Printf("writing activity history: %v", err)
	}
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

func (m *manager) track(name, action string) func(error) {
	m.note(name, action, "started", "")
	return func(err error) {
		if err != nil {
			m.note(name, action, "failed", describe(err))
			return
		}
		m.note(name, action, "completed", "")
	}
}
