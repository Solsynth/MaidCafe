package daemon

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// processHistoryDayFormat names one day's sample file (UTC), mirroring the
// metrics history layout.
const processHistoryDayFormat = "2006-01-02"

// processHistoryMaxSamples bounds the in-memory ring used when no storage
// directory is configured (tests and memory-only daemons).
const processHistoryMaxSamples = 1000

// processHistorySample is one per-watched-process usage sample. Threads is
// null on BSD/macOS hosts where ps has no nlwp column.
type processHistorySample struct {
	Name         string    `json:"name"`
	TS           time.Time `json:"ts"`
	CPUPercent   float64   `json:"cpu_percent"`
	RSSKb        int64     `json:"rss_kb"`
	ProcessCount int       `json:"process_count"`
	Threads      *int64    `json:"threads"`
}

// processHistoryStore persists per-watched-process usage samples as per-day
// JSONL files under the metrics history directory, pruned by retention.
// Without a storage directory it keeps a bounded in-memory ring instead, so
// the API stays functional on memory-only daemons and in tests.
type processHistoryStore struct {
	mu        sync.Mutex
	dir       string
	retention time.Duration
	ring      []processHistorySample
}

func newProcessHistoryStore(storageDir string, retentionDays int) *processHistoryStore {
	store := &processHistoryStore{
		dir:       storageDir,
		retention: time.Duration(retentionDays) * 24 * time.Hour,
	}
	if store.retention <= 0 {
		store.retention = 7 * 24 * time.Hour
	}
	if store.retention > 30*24*time.Hour {
		store.retention = 30 * 24 * time.Hour
	}
	return store
}

// Append records one sample, pruning expired day files when the sample lands
// in a new day. Safe for concurrent use.
func (s *processHistoryStore) Append(sample processHistorySample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == "" {
		s.ring = append(s.ring, sample)
		if len(s.ring) > processHistoryMaxSamples {
			s.ring = s.ring[len(s.ring)-processHistoryMaxSamples:]
		}
		return
	}
	if err := s.pruneLocked(sample.TS); err != nil {
		// Pruning is best-effort; a failed prune never drops the sample.
		_ = err
	}
	path := filepath.Join(s.dir, sample.TS.UTC().Format(processHistoryDayFormat)+".jsonl")
	data, err := json.Marshal(sample)
	if err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return
	}
	_ = file.Close()
}

// Query returns samples for name within [from, to] (nil bounds are open),
// newest last, capped at limit (0 = default 500, max 5000).
func (s *processHistoryStore) Query(name string, from, to *time.Time, limit int) []processHistorySample {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	if s.dir == "" {
		out := make([]processHistorySample, 0, 32)
		for _, sample := range s.ring {
			if sample.Name != name {
				continue
			}
			if from != nil && sample.TS.Before(*from) {
				continue
			}
			if to != nil && sample.TS.After(*to) {
				continue
			}
			out = append(out, sample)
		}
		return tailSamples(out, limit)
	}
	start := time.Time{}
	if from != nil {
		start = *from
	}
	end := time.Now().Add(24 * time.Hour)
	if to != nil {
		end = *to
	}
	out := make([]processHistorySample, 0, 64)
	for day := start.UTC().Truncate(24 * time.Hour); !day.After(end); day = day.Add(24 * time.Hour) {
		path := filepath.Join(s.dir, day.Format(processHistoryDayFormat)+".jsonl")
		file, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return out
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var sample processHistorySample
			if json.Unmarshal([]byte(line), &sample) != nil || sample.Name != name {
				continue
			}
			if from != nil && sample.TS.Before(*from) {
				continue
			}
			if to != nil && sample.TS.After(*to) {
				continue
			}
			out = append(out, sample)
		}
		_ = file.Close()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return tailSamples(out, limit)
}

// pruneLocked removes day files whose date is older than the retention
// window. Callers hold mu.
func (s *processHistoryStore) pruneLocked(now time.Time) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(s.dir, 0o750)
		}
		return err
	}
	cutoff := now.Add(-s.retention).UTC().Truncate(24 * time.Hour)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		day, err := time.Parse(processHistoryDayFormat, strings.TrimSuffix(entry.Name(), ".jsonl"))
		if err != nil || !day.Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// tailSamples returns the last limit samples, keeping order.
func tailSamples(samples []processHistorySample, limit int) []processHistorySample {
	if len(samples) <= limit {
		return samples
	}
	return samples[len(samples)-limit:]
}
