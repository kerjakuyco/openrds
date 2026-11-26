package main

import (
	"context"
	"errors"
	"log"
	"regexp"
	"slices"
	"strconv"
	"time"
)

const defaultBackupWindow = "03:00-03:30"

// How often automated backups may run, in hours. Each divides a day, so the
// backup times are the same every day.
var backupIntervals = []int{1, 2, 3, 4, 6, 12, 24}

// An instance's interval. 0, as in records from before intervals existed, is daily.
func backupInterval(hours int) time.Duration {
	if hours <= 0 {
		hours = 24
	}
	return time.Duration(hours) * time.Hour
}

var windowPattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])-([01][0-9]|2[0-3]):([0-5][0-9])$`)

// A daily UTC window as minutes after midnight. It may wrap past midnight.
func parseWindow(w string) (start, length int, ok bool) {
	p := windowPattern.FindStringSubmatch(w)
	if p == nil {
		return 0, 0, false
	}
	minutes := func(h, m string) int {
		hh, _ := strconv.Atoi(h)
		mm, _ := strconv.Atoi(m)
		return hh*60 + mm
	}
	start = minutes(p[1], p[2])
	length = (minutes(p[3], p[4]) - start + 1440) % 1440
	return start, length, length >= 30
}

// The latest opening of the backup window at or before now, and whether it
// is still open. The window opens daily at its start time and, with a shorter
// interval, again every interval after that. Each opening lasts the window's
// length, at most the interval.
func backupWindow(w string, intervalHours int, now time.Time) (opened time.Time, open bool) {
	start, length, ok := parseWindow(w)
	if !ok {
		start, length, _ = parseWindow(defaultBackupWindow)
	}
	every := backupInterval(intervalHours)
	now = now.UTC()
	first := now.Truncate(24 * time.Hour).Add(time.Duration(start) * time.Minute)
	if first.After(now) {
		first = first.AddDate(0, 0, -1)
	}
	opened = first.Add(now.Sub(first) / every * every)
	return opened, now.Before(opened.Add(min(time.Duration(length)*time.Minute, every)))
}

// When the next automated backup should start, given the latest one.
func nextBackup(window string, intervalHours int, latest, now time.Time) time.Time {
	opened, open := backupWindow(window, intervalHours, now)
	if open && latest.Before(opened) {
		return now
	}
	return opened.Add(backupInterval(intervalHours))
}

// Latest automated backup of each instance, ignoring older instances that used the same name.
func latestAutomated(records []record, backups []backup) map[string]time.Time {
	created := make(map[string]time.Time, len(records))
	for _, r := range records {
		created[r.Name] = r.Created
	}
	latest := make(map[string]time.Time)
	for _, b := range backups {
		if c, ok := created[b.Name]; ok && b.Kind == "automated" && !b.Created.Before(c) && b.Created.After(latest[b.Name]) {
			latest[b.Name] = b.Created
		}
	}
	return latest
}

// Claim the instances whose backup window is open and which have no automated
// backup from it yet. Each window gets one attempt; a busy instance is retried.
func (m *manager) dueBackups(ctx context.Context, now time.Time) []string {
	records, err := m.records()
	if err != nil {
		log.Printf("automated backups: %v", err)
		return nil
	}
	backups, err := m.backups()
	if err != nil {
		log.Printf("automated backups: %v", err)
		return nil
	}
	var states map[string]string
	var due []string
	latest := latestAutomated(records, backups)
	for _, r := range records {
		if r.BackupRetentionDays == 0 || r.PendingRestore {
			continue
		}
		opened, open := backupWindow(r.BackupWindow, r.BackupIntervalHours, now)
		if r.archiving() && !hasBase(backups, r) {
			// Point-in-time recovery starts with a base backup, so take one now,
			// trying once an hour if it fails.
			opened, open = now.Truncate(time.Hour), true
		} else if !open || !latest[r.Name].Before(opened) {
			continue
		}
		if states == nil {
			if states, err = m.states(ctx); err != nil {
				log.Printf("automated backups: cannot read database status: %v", err)
				return nil
			}
		}
		if states[r.Project] != "running" {
			continue // stopped databases are not backed up; a starting one is retried
		}
		m.mu.Lock()
		if !m.busy[r.Name] && !m.attempts[r.Project].Equal(opened) {
			m.attempts[r.Project] = opened
			due = append(due, r.Name)
		}
		m.mu.Unlock()
	}
	return due
}

// Whether the database's current data has a base backup to replay WAL onto.
func hasBase(backups []backup, r record) bool {
	return slices.ContainsFunc(backups, func(b backup) bool { return b.Format == "physical" && b.DataKey == r.diskKey() })
}

func (m *manager) automatedBackup(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	_, err := m.makeBackup(ctx, name, "automated")
	if errors.Is(err, errBusy) {
		if r, err := m.record(name); err == nil {
			m.mu.Lock()
			delete(m.attempts, r.Project) // another operation started first; try again next minute
			m.mu.Unlock()
		}
		return
	}
	if err != nil {
		log.Printf("automated backup of %s: %v", name, err)
	}
}

// Remove automated backups past their retention period, including those of
// deleted instances. Manual snapshots are kept until someone deletes them.
func (m *manager) expireBackups(ctx context.Context, now time.Time) {
	list, err := m.backups()
	if err != nil {
		log.Printf("expiring backups: %v", err)
		return
	}
	for _, b := range list {
		if b.Kind == "automated" && b.Expires != nil && now.After(*b.Expires) {
			if err := m.expire(ctx, b.ID, now); err != nil {
				log.Printf("expiring backup %s: %v", b.ID, err)
			}
		}
	}
	if list, err = m.backups(); err == nil {
		m.pruneWAL(ctx, list)
	}
}

// Run backup windows, expiry and health and disk checks once a minute until ctx ends.
func (m *manager) maintain(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		now := time.Now().UTC()
		m.expireBackups(ctx, now)
		for _, name := range m.dueBackups(ctx, now) {
			go m.automatedBackup(ctx, name)
		}
		go m.syncOffsite() // copies new backups and retries failed ones
		go m.watchHealth(ctx)
		go m.collectAllWAL(ctx)
		go m.watchReplication(ctx)
		m.watchDisk(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
