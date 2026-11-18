package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// LogEntry is one gateway runtime event. Entries never contain credentials,
// tokens, or cookies: HTTP rows log method, path, and status only.
type LogEntry struct {
	Time   time.Time      `json:"time"`
	Level  string         `json:"level"`
	Type   string         `json:"type"`
	Events string         `json:"message"`
	Fields map[string]any `json:"fields,omitempty"`
}

const (
	logRingSize   = 1000
	logFileMaxAge = 14 * 24 * time.Hour
)

type logStore struct {
	mu      sync.Mutex
	entries []LogEntry
	dir     string
	file    *os.File
	day     string
	closed  bool
}

func newLogStore(dir string) *logStore {
	return &logStore{dir: dir}
}

func sanitizeLogFields(fields map[string]any) map[string]any {
	if len(fields) == 0 {
		return nil
	}
	clean := make(map[string]any, len(fields))
	for key, value := range fields {
		switch value.(type) {
		case string, bool, int, int64, float64, time.Duration, nil:
			clean[key] = value
		default:
			encoded, err := json.Marshal(value)
			if err != nil {
				clean[key] = "?"
			} else {
				clean[key] = json.RawMessage(encoded)
			}
		}
	}
	return clean
}

// write appends to the in-memory ring and the daily JSONL file.
func (store *logStore) write(level, typ, message string, fields map[string]any) {
	entry := LogEntry{Time: time.Now().UTC(), Level: level, Type: typ, Events: message, Fields: sanitizeLogFields(fields)}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return
	}
	if len(store.entries) >= logRingSize {
		copy(store.entries, store.entries[len(store.entries)-logRingSize+1:])
		store.entries = store.entries[:logRingSize-1]
	}
	store.entries = append(store.entries, entry)
	store.appendFile(entry)
}

func (store *logStore) appendFile(entry LogEntry) {
	if store.dir == "" {
		return
	}
	day := entry.Time.Format("20060102")
	if store.file == nil || store.day != day {
		if store.file != nil {
			store.file.Close()
			store.file = nil
		}
		if err := os.MkdirAll(store.dir, 0700); err != nil {
			return
		}
		store.pruneOldFiles(day)
		path := filepath.Join(store.dir, "gateway-"+day+".log")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return
		}
		store.file, store.day = file, day
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_, _ = store.file.Write(append(data, '\n'))
}

func (store *logStore) pruneOldFiles(current string) {
	entries, err := os.ReadDir(store.dir)
	if err != nil {
		return
	}
	deadline := time.Now().Add(-logFileMaxAge)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "gateway-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(deadline) {
			continue
		}
		_ = os.Remove(filepath.Join(store.dir, name))
	}
	_ = current
}

func (store *logStore) query(limit int, typ string) []LogEntry {
	store.mu.Lock()
	defer store.mu.Unlock()
	selected := make([]LogEntry, 0, len(store.entries))
	for _, entry := range store.entries {
		if typ != "" && entry.Type != typ {
			continue
		}
		selected = append(selected, entry)
	}
	if limit > 0 && len(selected) > limit {
		selected = selected[len(selected)-limit:]
	}
	return selected
}

func (store *logStore) logFile() string {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.day == "" {
		return ""
	}
	return filepath.Join(store.dir, "gateway-"+store.day+".log")
}

func (store *logStore) close() {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.closed = true
	if store.file != nil {
		store.file.Close()
		store.file = nil
	}
}

func sortLogTypes(counts map[string]int) []string {
	types := make([]string, 0, len(counts))
	for typ := range counts {
		types = append(types, typ)
	}
	sort.Strings(types)
	return types
}
