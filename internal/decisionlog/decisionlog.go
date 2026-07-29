// Package decisionlog records every decision the trading engine makes —
// not just the trades that resulted — as an append-only JSON Lines file.
// This is the audit trail for improving the strategy later: for every
// symbol, on every tick, why it did or didn't trade, with the indicator
// values that drove the call.
package decisionlog

import (
	"encoding/json"
	"log/slog"
	"os"
	"time"
)

// Action is what the engine decided to do about a symbol on a given tick.
type Action string

const (
	Enter Action = "enter"
	Skip  Action = "skip"
)

type Decision struct {
	Time   time.Time `json:"time"`
	Symbol string    `json:"symbol"`
	Action Action    `json:"action"`
	Reason string    `json:"reason"`

	// Indicator snapshot, when available (omitted for pre-signal skips like
	// "already held").
	Price   float64 `json:"price,omitempty"`
	EMAFast float64 `json:"ema_fast,omitempty"`
	EMASlow float64 `json:"ema_slow,omitempty"`
	RSI     float64 `json:"rsi,omitempty"`
	ATR     float64 `json:"atr,omitempty"`

	Uptrend  *bool `json:"uptrend,omitempty"`
	Momentum *bool `json:"momentum,omitempty"`

	// Populated only when Action == Enter.
	Qty     float64 `json:"qty,omitempty"`
	Stop    float64 `json:"stop,omitempty"`
	Take    float64 `json:"take,omitempty"`
	OrderID string  `json:"order_id,omitempty"`
}

type Logger struct {
	path string
}

func Open(path string) *Logger {
	return &Logger{path: path}
}

// Log appends d to the log file. Logging is best-effort: a failure to write
// is reported via slog but never blocks the engine's actual trading logic.
func (l *Logger) Log(d Decision) {
	if d.Time.IsZero() {
		d.Time = time.Now()
	}
	data, err := json.Marshal(d)
	if err != nil {
		slog.Error("failed to marshal decision", "err", err)
		return
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Error("failed to open decision log", "err", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		slog.Error("failed to write decision log", "err", err)
	}
}

// Bool returns a pointer to b, for the optional bool fields on Decision.
func Bool(b bool) *bool { return &b }
