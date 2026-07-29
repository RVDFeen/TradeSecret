// Package tradelog persists every trade fill to a local append-only JSON
// Lines file, so there's a durable record of what actually happened —
// including fills from Alpaca's server-side stop-loss/take-profit orders,
// which happen whether or not the bot is running to see it.
package tradelog

import (
	"bufio"
	"encoding/json"
	"os"
	"time"
)

type Entry struct {
	ID      string    `json:"id"`
	Time    time.Time `json:"time"`
	Symbol  string    `json:"symbol"`
	Side    string    `json:"side"`
	Qty     float64   `json:"qty"`
	Price   float64   `json:"price"`
	OrderID string    `json:"order_id"`
}

// Logger appends new entries to path, deduping by ID so re-querying an
// overlapping time window never double-logs the same fill.
type Logger struct {
	path     string
	seen     map[string]bool
	lastTime time.Time
}

// Open loads any existing log at path (if present) to seed the dedup set and
// the last-seen timestamp, so a restart resumes from where it left off
// instead of re-fetching (or missing) history.
func Open(path string) (*Logger, error) {
	l := &Logger{path: path, seen: map[string]bool{}}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var e Entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue // skip a malformed line rather than fail the whole log
		}
		l.seen[e.ID] = true
		if e.Time.After(l.lastTime) {
			l.lastTime = e.Time
		}
	}
	return l, scanner.Err()
}

// LastTime is the transaction time of the most recently logged fill, or the
// zero Time if nothing has been logged yet.
func (l *Logger) LastTime() time.Time {
	return l.lastTime
}

// Append writes any entries not already logged (by ID) to the file and
// updates the in-memory dedup state.
func (l *Logger) Append(entries []Entry) error {
	var toWrite []Entry
	for _, e := range entries {
		if l.seen[e.ID] {
			continue
		}
		toWrite = append(toWrite, e)
	}
	if len(toWrite) == 0 {
		return nil
	}

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	for _, e := range toWrite {
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(data, '\n')); err != nil {
			return err
		}
		l.seen[e.ID] = true
		if e.Time.After(l.lastTime) {
			l.lastTime = e.Time
		}
	}
	return w.Flush()
}
