// Package universe picks which symbols the live engine actually watches each
// day, out of everything tradable. Scanning every US equity every poll isn't
// possible within Alpaca's rate limits, so the expensive part — ranking a
// broad universe — runs once a day, and the frequent intraday polling only
// ever looks at that day's cached shortlist.
package universe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"tradebot/internal/bar"
)

const cachePath = "data/universe.json"

type Cache struct {
	Date    string   `json:"date"` // "2006-01-02"
	Symbols []string `json:"symbols"`
}

// Today returns the current calendar date in the cache's date format.
func Today() string {
	return time.Now().Format("2006-01-02")
}

// Load returns today's cached shortlist, if one exists for today specifically
// — a cache from a previous day is treated as absent, not stale-but-usable.
func Load() (Cache, bool) {
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return Cache{}, false
	}
	var c Cache
	if err := json.Unmarshal(data, &c); err != nil {
		return Cache{}, false
	}
	if c.Date != Today() {
		return Cache{}, false
	}
	return c, true
}

// Save persists today's shortlist so a restart later the same day doesn't
// need to recompute it.
func Save(symbols []string) error {
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(Cache{Date: Today(), Symbols: symbols})
	if err != nil {
		return err
	}
	return os.WriteFile(cachePath, data, 0o644)
}

// RankByLiquidity scores each symbol by its average dollar volume
// (close price × volume, averaged across the given bars) and returns the
// topN symbols, most liquid first. Symbols with no bars are dropped rather
// than scored as zero, since "no data" and "genuinely illiquid" aren't the
// same thing and shouldn't be sorted together.
func RankByLiquidity(barsBySymbol map[string][]bar.Bar, topN int) []string {
	type scored struct {
		symbol string
		score  float64
	}
	scores := make([]scored, 0, len(barsBySymbol))
	for sym, bars := range barsBySymbol {
		if len(bars) == 0 {
			continue
		}
		total := 0.0
		for _, b := range bars {
			total += b.Close * b.Volume
		}
		scores = append(scores, scored{symbol: sym, score: total / float64(len(bars))})
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].score > scores[j].score })

	n := topN
	if n > len(scores) {
		n = len(scores)
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = scores[i].symbol
	}
	return out
}
