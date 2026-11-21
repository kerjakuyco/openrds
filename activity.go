package main

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"
)

type event struct {
	At     time.Time `json:"at"`
	Name   string    `json:"name"`
	Action string    `json:"action"`
	Result string    `json:"result"`
}

func (m *manager) readActivity() ([]event, error) {
	items := []event{}
	err := readJSON(filepath.Join(m.dir, "activity.json"), &items)
	if errors.Is(err, os.ErrNotExist) {
		return items, nil
	}
	return items, err
}

func (m *manager) note(name, action, result string) {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()
	items, err := m.readActivity()
	if err == nil {
		items = append([]event{{time.Now().UTC(), name, action, result}}, items...)
		if len(items) > 200 {
			items = items[:200]
		}
		err = writeJSON(filepath.Join(m.dir, "activity.json"), items)
	}
	if err != nil {
		log.Printf("writing activity history: %v", err)
	}
}

func (m *manager) track(name, action string) func(error) {
	m.note(name, action, "started")
	return func(err error) {
		result := "completed"
		if err != nil {
			result = "failed"
		}
		m.note(name, action, result)
	}
}
