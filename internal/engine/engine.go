// Package engine ties the broker, strategy, and risk manager together into
// a poll loop suitable for paper (or, in principle, live) trading.
package engine

import (
	"context"
	"log/slog"
	"time"

	"tradebot/internal/broker"
	"tradebot/internal/config"
	"tradebot/internal/risk"
	"tradebot/internal/strategy"
)

const lookbackDays = 120 // enough history for EMA(26)/RSI(14)/ATR(14) to settle

type Engine struct {
	cfg    *config.Config
	broker *broker.Broker
	risk   risk.Manager
	params strategy.Params

	dayStartEquity float64
	dayStartDate   string
	haltedToday    bool
}

func New(cfg *config.Config, b *broker.Broker) *Engine {
	return &Engine{
		cfg:    cfg,
		broker: b,
		risk: risk.Manager{
			RiskPerTradePct:   cfg.RiskPerTradePct,
			MaxPositionPct:    cfg.MaxPositionPct,
			MaxPositions:      cfg.MaxPositions,
			DailyLossLimitPct: cfg.DailyLossLimitPct,
		},
		params: strategy.DefaultParams(),
	}
}

// Run blocks, polling at cfg.PollInterval while the market is open, until ctx
// is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	slog.Info("engine starting", "watchlist", e.cfg.Watchlist, "poll_interval", e.cfg.PollInterval)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		clock, err := e.broker.GetClock()
		if err != nil {
			slog.Error("get clock failed", "err", err)
			if !sleepCtx(ctx, e.cfg.PollInterval) {
				return ctx.Err()
			}
			continue
		}

		if !clock.IsOpen {
			wait := time.Until(clock.NextOpen)
			slog.Info("market closed, sleeping until next open", "next_open", clock.NextOpen)
			if wait > e.cfg.PollInterval {
				wait = e.cfg.PollInterval
			}
			if !sleepCtx(ctx, wait) {
				return ctx.Err()
			}
			continue
		}

		e.tick(ctx)

		if !sleepCtx(ctx, e.cfg.PollInterval) {
			return ctx.Err()
		}
	}
}

// RunOnce performs a single evaluation pass immediately (used for --once and
// for manual testing) regardless of market hours, and returns.
func (e *Engine) RunOnce(ctx context.Context) {
	e.tick(ctx)
}

func (e *Engine) tick(ctx context.Context) {
	acc, err := e.broker.GetAccount()
	if err != nil {
		slog.Error("get account failed", "err", err)
		return
	}
	e.rolloverDayIfNeeded(acc.Equity)

	if e.risk.DailyLossBreached(e.dayStartEquity, acc.Equity) && !e.haltedToday {
		slog.Warn("daily loss limit breached — halting new entries and cancelling open orders for the rest of the day",
			"day_start_equity", e.dayStartEquity, "current_equity", acc.Equity)
		if err := e.broker.CancelAllOrders(); err != nil {
			slog.Error("cancel all orders failed", "err", err)
		}
		e.haltedToday = true
	}

	positions, err := e.broker.OpenPositionSymbols()
	if err != nil {
		slog.Error("get positions failed", "err", err)
		return
	}
	openOrders, err := e.broker.OpenOrderSymbols()
	if err != nil {
		slog.Error("get open orders failed", "err", err)
		return
	}

	if e.haltedToday {
		slog.Info("halted for the day, skipping new entries", "open_positions", len(positions))
		return
	}

	if !e.risk.CanOpenNewPosition(len(positions)) {
		slog.Info("max concurrent positions reached, skipping new entries", "open_positions", len(positions))
		return
	}

	for _, symbol := range e.cfg.Watchlist {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if positions[symbol] || openOrders[symbol] {
			continue // already in or entering this name
		}
		if !e.risk.CanOpenNewPosition(len(positions)) {
			break // filled up mid-loop
		}

		e.evaluateAndMaybeEnter(symbol, acc)
	}
}

func (e *Engine) evaluateAndMaybeEnter(symbol string, acc broker.AccountSnapshot) {
	bars, err := e.broker.GetDailyBars(symbol, lookbackDays)
	if err != nil {
		slog.Error("get bars failed", "symbol", symbol, "err", err)
		return
	}
	sig, ok := strategy.Evaluate(bars, e.params)
	if !ok {
		slog.Warn("not enough bar history to evaluate", "symbol", symbol, "bars", len(bars))
		return
	}
	if !sig.ShouldEnter {
		slog.Debug("no entry signal", "symbol", symbol, "uptrend", sig.Uptrend, "momentum", sig.Momentum, "rsi", sig.RSI)
		return
	}

	price, err := e.broker.GetLatestPrice(symbol)
	if err != nil {
		slog.Error("get latest price failed", "symbol", symbol, "err", err)
		return
	}

	qty := e.risk.PositionSize(acc.Equity, acc.BuyingPower, price, sig.StopPrice)
	if qty <= 0 {
		slog.Info("position size computed to zero, skipping", "symbol", symbol, "price", price, "stop", sig.StopPrice)
		return
	}

	slog.Info("entering position", "symbol", symbol, "qty", qty, "price", price,
		"stop", sig.StopPrice, "take", sig.TakePrice, "rsi", sig.RSI, "atr", sig.ATR)

	order, err := e.broker.PlaceBracketBuy(symbol, qty, sig.StopPrice, sig.TakePrice)
	if err != nil {
		slog.Error("place order failed", "symbol", symbol, "err", err)
		return
	}
	slog.Info("order submitted", "symbol", symbol, "order_id", order.ID)
}

func (e *Engine) rolloverDayIfNeeded(currentEquity float64) {
	today := time.Now().Format("2006-01-02")
	if e.dayStartDate != today {
		e.dayStartDate = today
		e.dayStartEquity = currentEquity
		e.haltedToday = false
		slog.Info("new trading day", "date", today, "start_equity", currentEquity)
	}
}

// sleepCtx sleeps for d or until ctx is cancelled, returning false if cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
