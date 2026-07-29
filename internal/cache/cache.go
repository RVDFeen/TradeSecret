// Package cache persists historical daily bars to local JSON files so
// repeated backtest runs (e.g. parameter sweeps) don't re-fetch the same
// history from Alpaca's market data API every time.
package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"tradebot/internal/bar"
)

const dir = "data/cache"

// slack tolerates the requested window being a couple of days inside the
// cached window (weekends/holidays mean bar dates don't line up exactly with
// calendar dates).
const slack = 3 * 24 * time.Hour

// path is keyed by symbol AND timeframe (e.g. "AAPL_1Hour.json") so daily and
// hourly bars for the same symbol never collide in the same cache file.
func path(symbol, timeframeKey string) string {
	return filepath.Join(dir, symbol+"_"+timeframeKey+".json")
}

func load(symbol, timeframeKey string) []bar.Bar {
	data, err := os.ReadFile(path(symbol, timeframeKey))
	if err != nil {
		return nil
	}
	var bars []bar.Bar
	if err := json.Unmarshal(data, &bars); err != nil {
		return nil
	}
	return bars
}

func save(symbol, timeframeKey string, bars []bar.Bar) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(bars)
	if err != nil {
		return err
	}
	return os.WriteFile(path(symbol, timeframeKey), data, 0o644)
}

func covers(bars []bar.Bar, start, end time.Time) bool {
	if len(bars) == 0 {
		return false
	}
	return !bars[0].Time.After(start.Add(slack)) && !bars[len(bars)-1].Time.Before(end.Add(-slack))
}

func filterRange(bars []bar.Bar, start, end time.Time) []bar.Bar {
	out := make([]bar.Bar, 0, len(bars))
	for _, b := range bars {
		if !b.Time.Before(start) && !b.Time.After(end) {
			out = append(out, b)
		}
	}
	return out
}

// mergeSorted dedupes by exact bar timestamp (not calendar day — a day holds
// many distinct bars at sub-daily timeframes) and returns the union sorted
// oldest first.
func mergeSorted(a, b []bar.Bar) []bar.Bar {
	byTime := make(map[int64]bar.Bar, len(a)+len(b))
	for _, x := range a {
		byTime[x.Time.UnixNano()] = x
	}
	for _, x := range b {
		byTime[x.Time.UnixNano()] = x
	}
	out := make([]bar.Bar, 0, len(byTime))
	for _, x := range byTime {
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

// FetchFunc fetches bars for symbol in [start,end] from the real data source.
type FetchFunc func(symbol string, start, end time.Time) ([]bar.Bar, error)

// GetBars returns bars for symbol covering [start,end], serving from the
// local on-disk cache (keyed by symbol + timeframeKey) when possible and only
// calling fetch to fill gaps (a brand-new symbol, or a window extending past
// what's cached).
func GetBars(symbol, timeframeKey string, start, end time.Time, fetch FetchFunc) ([]bar.Bar, error) {
	existing := load(symbol, timeframeKey)
	if covers(existing, start, end) {
		return filterRange(existing, start, end), nil
	}

	fetchStart := start
	if len(existing) > 0 && existing[0].Time.Before(start) {
		fetchStart = existing[0].Time // keep whatever older history we already had
	}
	fetched, err := fetch(symbol, fetchStart, end)
	if err != nil {
		if len(existing) > 0 {
			return filterRange(existing, start, end), nil // serve stale cache rather than fail outright
		}
		return nil, err
	}

	merged := mergeSorted(existing, fetched)
	_ = save(symbol, timeframeKey, merged) // caching is a performance optimization; failure to persist isn't fatal

	return filterRange(merged, start, end), nil
}
