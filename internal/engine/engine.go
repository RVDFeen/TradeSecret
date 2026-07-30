// Package engine ties the broker, strategy, and risk manager together into
// a poll loop suitable for paper (or, in principle, live) trading.
package engine

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"tradebot/internal/bar"
	"tradebot/internal/broker"
	"tradebot/internal/config"
	"tradebot/internal/decisionlog"
	"tradebot/internal/indicators"
	"tradebot/internal/ratelimit"
	"tradebot/internal/risk"
	"tradebot/internal/strategy"
	"tradebot/internal/timeframe"
	"tradebot/internal/tradelog"
	"tradebot/internal/universe"
)

const (
	tradeLogPath    = "data/trades.jsonl"
	decisionLogPath = "data/decisions.jsonl"
)

type Engine struct {
	cfg          *config.Config
	broker       *broker.Broker
	risk         risk.Manager
	params       strategy.Params
	lookbackDays int // calendar days of history to fetch per evaluation
	tradeLog     *tradelog.Logger
	decisionLog  *decisionlog.Logger

	currentUniverse []string // symbols actually scanned this run (static watchlist, or today's dynamic shortlist)
	universeDate    string   // calendar date currentUniverse was computed for, when dynamic
	scanOffset      int      // round-robin position into currentUniverse for candidate-scan throttling

	dayStartEquity float64
	dayStartDate   string
	haltedToday    bool
}

func New(cfg *config.Config, b *broker.Broker) (*Engine, error) {
	params := strategy.DefaultDailyParams()
	lookbackDays := 120 // enough calendar days for EMA(50)/RSI(14)/ATR(14) to settle on daily bars
	switch cfg.Timeframe.Unit {
	case timeframe.Minute:
		params = strategy.DefaultMinuteParams()
		lookbackDays = 5 // ~1000+ one-minute bars, comfortably past EMA(60)'s settling window
	case timeframe.Hour:
		params = strategy.DefaultHourlyParams()
		lookbackDays = 30 // ~195 hourly bars, comfortably past EMA(21)'s settling window
	}

	if err := os.MkdirAll(filepath.Dir(tradeLogPath), 0o755); err != nil {
		return nil, err
	}
	tl, err := tradelog.Open(tradeLogPath)
	if err != nil {
		return nil, err
	}

	return &Engine{
		cfg:    cfg,
		broker: b,
		risk: risk.Manager{
			RiskPerTradePct:   cfg.RiskPerTradePct,
			MaxPositionPct:    cfg.MaxPositionPct,
			MaxPositions:      cfg.MaxPositions,
			DailyLossLimitPct: cfg.DailyLossLimitPct,
		},
		params:       params,
		lookbackDays: lookbackDays,
		tradeLog:     tl,
		decisionLog:  decisionlog.Open(decisionLogPath),
	}, nil
}

// Run blocks, polling at cfg.PollInterval while the market is open, until ctx
// is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	if e.cfg.DynamicUniverse {
		slog.Info("engine starting", "universe_mode", "dynamic", "universe_size", e.cfg.UniverseSize, "poll_interval", e.cfg.PollInterval)
	} else {
		slog.Info("engine starting", "watchlist", e.cfg.Watchlist, "poll_interval", e.cfg.PollInterval)
	}

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
	// Runs first and unconditionally: every held position — including ones
	// this bot didn't open itself — must have a live protective stop. Track
	// the real cost so candidate-scanning below is budgeted off what this
	// actually spent, not a worst-case guess.
	callsSpent := ratelimit.AlwaysEveryTickCalls
	fixed := e.ensurePositionsProtected()
	callsSpent += ratelimit.PerUnprotectedPositionCalls * float64(fixed)

	// Also unconditional: record every fill since the last tick, including
	// ones from a stop-loss/take-profit that triggered server-side while this
	// bot wasn't even running to see it happen.
	e.logNewFills()

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
		// CancelAllOrders just stripped every position's protective stop too
		// (they're the same open orders); reattach immediately rather than
		// leaving positions naked until the next poll.
		fixedAgain := e.ensurePositionsProtected()
		callsSpent += ratelimit.PerUnprotectedPositionCalls * float64(fixedAgain)
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

	symbols := e.symbolsToScan()

	if e.haltedToday {
		slog.Info("halted for the day, skipping new entries", "open_positions", len(positions))
		for _, symbol := range symbols {
			e.decisionLog.Log(decisionlog.Decision{Symbol: symbol, Action: decisionlog.Skip, Reason: "halted_daily_loss"})
		}
		return
	}

	if !e.risk.CanOpenNewPosition(len(positions)) && !e.cfg.EnableRotation {
		// With rotation disabled, there's nothing a full slate of positions
		// could lead to, so skip scanning entirely rather than spend the
		// rate-limit budget on candidates that can't be acted on regardless.
		// With rotation enabled, keep going: a strong-enough candidate might
		// still justify closing a weaker holding for it.
		slog.Info("max concurrent positions reached, skipping new entries", "open_positions", len(positions))
		for _, symbol := range symbols {
			e.decisionLog.Log(decisionlog.Decision{Symbol: symbol, Action: decisionlog.Skip, Reason: "max_positions_reached"})
		}
		return
	}

	// Sort candidates needing an actual bars-fetch call away from ones we can
	// skip for free (already held or already have a pending order) — only
	// the former count against the rate-limit scan budget below. Protecting
	// what's already held stays completely unaffected by any of this: it
	// already ran in full, above, before this function even looked at
	// candidates.
	var toScan []string
	for _, symbol := range symbols {
		if positions[symbol] || openOrders[symbol] {
			e.decisionLog.Log(decisionlog.Decision{Symbol: symbol, Action: decisionlog.Skip, Reason: "already_held_or_pending"})
			continue
		}
		toScan = append(toScan, symbol)
	}

	// Rate-limit budget: only scan as many candidates this tick as is safe
	// given what's actually been spent so far plus a reserve for filling
	// every remaining slot, rotating through the rest so everything still
	// gets covered over subsequent ticks. Position protection above was
	// never part of this budget and always ran in full regardless.
	availableSlots := e.risk.MaxPositions - len(positions)
	maxScan := ratelimit.MaxCandidatesPerTick(e.cfg.PollInterval, callsSpent, availableSlots)
	window, rest := rotatingWindow(toScan, e.scanOffset, maxScan)
	if len(rest) > 0 {
		slog.Info("rate-limit budget: scanning a subset of candidates this tick, rotating through the rest",
			"scanning", len(window), "deferred", len(rest), "total_candidates", len(toScan))
		for _, symbol := range rest {
			e.decisionLog.Log(decisionlog.Decision{Symbol: symbol, Action: decisionlog.Skip, Reason: "deferred_rate_limit_rotation"})
		}
	}
	e.scanOffset = (e.scanOffset + len(window)) % max(len(toScan), 1)

	// Pass 1: evaluate this tick's window of candidates, and collect the
	// ones whose signal says to enter. Nothing gets bought yet — this is
	// just gathering the field before ranking it.
	type candidate struct {
		symbol string
		sig    strategy.Signal
	}
	var candidates []candidate
	for _, symbol := range window {
		select {
		case <-ctx.Done():
			return
		default:
		}

		sig, ok := e.evaluateSignal(symbol)
		if !ok {
			continue // evaluateSignal already logged why
		}
		if !sig.ShouldEnter {
			slog.Debug("no entry signal", "symbol", symbol, "uptrend", sig.Uptrend, "momentum", sig.Momentum, "rsi", sig.RSI)
			e.decisionLog.Log(decisionlog.Decision{
				Symbol: symbol, Action: decisionlog.Skip, Reason: "no_signal",
				Price: sig.Price, EMAFast: sig.EMAFast, EMASlow: sig.EMASlow, RSI: sig.RSI, ATR: sig.ATR,
				Uptrend: decisionlog.Bool(sig.Uptrend), Momentum: decisionlog.Bool(sig.Momentum),
			})
			continue
		}
		candidates = append(candidates, candidate{symbol: symbol, sig: sig})
	}

	// Pass 2: rank qualifying candidates by trend strength and take the
	// strongest ones first, up to whatever slots MAX_POSITIONS still allows.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].sig.TrendStrength() > candidates[j].sig.TrendStrength()
	})

	// bestUnfilled is the highest-ranked candidate that qualified but didn't
	// end up entered this tick — whether because every slot was full, or
	// because it made it to enterPosition but couldn't actually be sized or
	// filled (most commonly: not enough buying power, which happens well
	// before MAX_POSITIONS is reached once a few positions have absorbed
	// most of the account). Feeds the rotation check below.
	var bestUnfilled *candidate
	noteUnfilled := func(c candidate) {
		if bestUnfilled == nil {
			bestUnfilled = &c
		}
	}

	for i, c := range candidates {
		if !e.risk.CanOpenNewPosition(len(positions)) {
			// Every remaining candidate loses out to higher-ranked ones (or
			// ones already held) hitting the cap first — log them as a group
			// rather than silently dropping them from the record.
			for _, rest := range candidates[i:] {
				e.decisionLog.Log(decisionlog.Decision{
					Symbol: rest.symbol, Action: decisionlog.Skip, Reason: "ranked_lower_max_positions",
					Price: rest.sig.Price, EMAFast: rest.sig.EMAFast, EMASlow: rest.sig.EMASlow,
					RSI: rest.sig.RSI, ATR: rest.sig.ATR,
				})
			}
			noteUnfilled(candidates[i])
			break
		}
		if e.enterPosition(c.symbol, c.sig, &acc) {
			// Mark it held immediately: len(positions) is what the guard
			// above checks, and it must reflect entries placed earlier in
			// this very pass — otherwise the position cap only works across
			// ticks, not within one, and a single tick can open far more
			// than MAX_POSITIONS.
			positions[c.symbol] = true
		} else {
			// enterPosition already logged the specific reason (zero size,
			// buying power, order error, ...).
			noteUnfilled(c)
		}
	}

	if e.cfg.EnableRotation && bestUnfilled != nil {
		e.maybeRotate(positions, bestUnfilled.symbol, bestUnfilled.sig)
	}
}

// maybeRotate closes the weakest currently-held position if candidateSig is
// clearly stronger (by more than ROTATION_MARGIN in TrendStrength) than that
// weakest holding — freeing both a slot and capital for it. Doesn't retry
// entering candidateSymbol itself this same tick; the next tick's normal
// scan picks it up once the close has actually settled. Opt-in
// (ENABLE_ROTATION) and unvalidated by backtest — see README.
func (e *Engine) maybeRotate(positions map[string]bool, candidateSymbol string, candidateSig strategy.Signal) {
	held, err := e.broker.GetOpenPositions()
	if err != nil {
		slog.Error("get open positions failed for rotation check", "err", err)
		return
	}

	type heldSignal struct {
		symbol string
		sig    strategy.Signal
	}
	var weakest *heldSignal
	for _, pos := range held {
		bars, err := e.broker.GetRecentBars(pos.Symbol, e.cfg.Timeframe, e.lookbackDays)
		if err != nil {
			slog.Error("get bars failed while evaluating rotation candidate", "symbol", pos.Symbol, "err", err)
			continue
		}
		sig, ok := strategy.Evaluate(bars, e.params)
		if !ok {
			continue
		}
		if weakest == nil || sig.TrendStrength() < weakest.sig.TrendStrength() {
			weakest = &heldSignal{symbol: pos.Symbol, sig: sig}
		}
	}
	if weakest == nil {
		return
	}

	if candidateSig.TrendStrength() < weakest.sig.TrendStrength()+e.cfg.RotationMargin {
		slog.Debug("rotation considered, not clearly better", "weakest_held", weakest.symbol,
			"weakest_strength", weakest.sig.TrendStrength(), "candidate", candidateSymbol, "candidate_strength", candidateSig.TrendStrength())
		return
	}

	slog.Info("rotating: closing weaker position for a clearly stronger candidate",
		"closing", weakest.symbol, "closing_strength", weakest.sig.TrendStrength(),
		"candidate", candidateSymbol, "candidate_strength", candidateSig.TrendStrength())
	// The position's own bracket (stop/take) legs hold a claim on its
	// shares; cancel those first or ClosePosition fails with "insufficient
	// qty available" even though the position is clearly ours to sell.
	if err := e.broker.CancelOrdersForSymbol(weakest.symbol); err != nil {
		slog.Error("failed to cancel existing orders before rotation close", "symbol", weakest.symbol, "err", err)
		return
	}
	if err := e.broker.ClosePosition(weakest.symbol); err != nil {
		slog.Error("failed to close position for rotation", "symbol", weakest.symbol, "err", err)
		return
	}
	e.decisionLog.Log(decisionlog.Decision{
		Symbol: weakest.symbol, Action: decisionlog.Skip, Reason: "rotated_out_for_stronger_candidate: " + candidateSymbol,
		Price: weakest.sig.Price, EMAFast: weakest.sig.EMAFast, EMASlow: weakest.sig.EMASlow,
	})
	delete(positions, weakest.symbol)
}

// symbolsToScan returns the static watchlist, or — in dynamic-universe mode
// — today's liquidity-ranked shortlist, refreshing it once per calendar day.
func (e *Engine) symbolsToScan() []string {
	if !e.cfg.DynamicUniverse {
		return e.cfg.Watchlist
	}

	today := universe.Today()
	if e.universeDate == today && len(e.currentUniverse) > 0 {
		return e.currentUniverse
	}

	if cached, ok := universe.Load(); ok {
		slog.Info("loaded today's universe from cache", "size", len(cached.Symbols))
		e.currentUniverse, e.universeDate = cached.Symbols, today
		return e.currentUniverse
	}

	slog.Info("refreshing daily universe — ranking tradable symbols by liquidity", "target_size", e.cfg.UniverseSize)
	symbols, err := e.refreshUniverse()
	if err != nil {
		slog.Error("universe refresh failed — falling back to previous/static list", "err", err)
		if len(e.currentUniverse) > 0 {
			return e.currentUniverse
		}
		return e.cfg.Watchlist
	}
	slog.Info("universe refreshed", "size", len(symbols))
	e.currentUniverse, e.universeDate = symbols, today
	return e.currentUniverse
}

// universeLiquidityLookbackDays is how much daily-bar history feeds the
// liquidity ranking — a couple weeks smooths out single-day noise without
// needing much data.
const universeLiquidityLookbackDays = 15

func (e *Engine) refreshUniverse() ([]string, error) {
	tradable, err := e.broker.GetTradableSymbols()
	if err != nil {
		return nil, err
	}
	barsBySymbol, err := e.broker.GetMultiRecentDailyBars(tradable, universeLiquidityLookbackDays)
	if err != nil {
		return nil, err
	}
	top := universe.RankByLiquidity(barsBySymbol, e.cfg.UniverseSize)
	if err := universe.Save(top); err != nil {
		slog.Error("failed to persist universe cache", "err", err) // non-fatal, just means recompute next restart
	}
	return top, nil
}

// evaluateSignal fetches bars and computes the strategy signal for symbol,
// logging (and returning false for) the cases where it can't produce one.
func (e *Engine) evaluateSignal(symbol string) (strategy.Signal, bool) {
	bars, err := e.broker.GetRecentBars(symbol, e.cfg.Timeframe, e.lookbackDays)
	if err != nil {
		slog.Error("get bars failed", "symbol", symbol, "err", err)
		e.decisionLog.Log(decisionlog.Decision{Symbol: symbol, Action: decisionlog.Skip, Reason: "bars_fetch_error: " + err.Error()})
		return strategy.Signal{}, false
	}
	sig, ok := strategy.Evaluate(bars, e.params)
	if !ok {
		slog.Warn("not enough bar history to evaluate", "symbol", symbol, "bars", len(bars))
		e.decisionLog.Log(decisionlog.Decision{Symbol: symbol, Action: decisionlog.Skip, Reason: "insufficient_bars"})
		return strategy.Signal{}, false
	}
	return sig, true
}

// enterPosition sizes and places an entry order for a symbol whose signal
// already said to enter, returning true if an order was actually submitted.
// acc is a pointer because BuyingPower is drawn down on a successful order —
// otherwise every candidate in a multi-entry tick sizes against the same
// stale pre-tick figure and later ones get rejected by Alpaca for buying
// power the earlier ones already spent.
func (e *Engine) enterPosition(symbol string, sig strategy.Signal, acc *broker.AccountSnapshot) bool {
	signalDecision := decisionlog.Decision{
		Symbol: symbol, Price: sig.Price, EMAFast: sig.EMAFast, EMASlow: sig.EMASlow,
		RSI: sig.RSI, ATR: sig.ATR, Uptrend: decisionlog.Bool(sig.Uptrend), Momentum: decisionlog.Bool(sig.Momentum),
	}

	price, err := e.broker.GetLatestPrice(symbol)
	if err != nil {
		slog.Error("get latest price failed", "symbol", symbol, "err", err)
		signalDecision.Action, signalDecision.Reason = decisionlog.Skip, "price_fetch_error: "+err.Error()
		e.decisionLog.Log(signalDecision)
		return false
	}

	// Re-anchor stop/take to the live price just fetched, not sig.Price (from
	// the bar the signal was computed on) — preserve the ATR-based distance,
	// but the absolute levels must be relative to what we're actually about
	// to pay. Otherwise, on a fast-moving bar, price can already have run
	// past the stale take-profit level by the time the order reaches Alpaca,
	// which rejects it ("take_profit.limit_price must be >= base_price").
	stopDistance := sig.Price - sig.StopPrice
	takeDistance := sig.TakePrice - sig.Price
	stopPrice := price - stopDistance
	takePrice := price + takeDistance

	qty := e.risk.PositionSize(acc.Equity, acc.BuyingPower, price, stopPrice)
	if qty <= 0 {
		slog.Info("position size computed to zero, skipping", "symbol", symbol, "price", price, "stop", stopPrice)
		signalDecision.Action, signalDecision.Reason = decisionlog.Skip, "zero_position_size"
		e.decisionLog.Log(signalDecision)
		return false
	}

	slog.Info("entering position", "symbol", symbol, "qty", qty, "price", price,
		"stop", stopPrice, "take", takePrice, "rsi", sig.RSI, "atr", sig.ATR)

	order, err := e.broker.PlaceBracketBuy(symbol, qty, stopPrice, takePrice)
	if err != nil {
		slog.Error("place order failed", "symbol", symbol, "err", err)
		signalDecision.Action, signalDecision.Reason = decisionlog.Skip, "order_place_error: "+err.Error()
		e.decisionLog.Log(signalDecision)
		return false
	}
	slog.Info("order submitted", "symbol", symbol, "order_id", order.ID)
	signalDecision.Action, signalDecision.Reason = decisionlog.Enter, "signal"
	signalDecision.Qty, signalDecision.Stop, signalDecision.Take, signalDecision.OrderID = float64(qty), stopPrice, takePrice, order.ID
	e.decisionLog.Log(signalDecision)
	// Draw down the shared snapshot so the next candidate ranked in this same
	// tick sizes against what's actually left, not what was available before
	// this order spent it.
	acc.BuyingPower -= float64(qty) * price
	return true
}

// ensurePositionsProtected scans every currently held position — regardless
// of whether this bot placed it — and attaches an ATR-based protective
// stop/take-profit OCO to any that don't already have a live stop order.
// This is what keeps a manually-opened position, or one whose bracket leg got
// cancelled some other way, from sitting completely unprotected. Returns how
// many positions it actually attempted to fix, so the caller can budget the
// rest of the tick's rate-limit usage off the real cost, not a guess.
func (e *Engine) ensurePositionsProtected() int {
	positions, err := e.broker.GetOpenPositions()
	if err != nil {
		slog.Error("get open positions failed", "err", err)
		return 0
	}
	if len(positions) == 0 {
		return 0
	}

	symbols := make([]string, len(positions))
	for i, p := range positions {
		symbols[i] = p.Symbol
	}
	protected, err := e.broker.HasLiveProtectiveStop(symbols)
	if err != nil {
		slog.Error("check protective stops failed", "err", err)
		return 0
	}

	fixed := 0
	for _, pos := range positions {
		if protected[pos.Symbol] {
			continue
		}
		slog.Warn("position has no live protective stop — attaching one now", "symbol", pos.Symbol, "qty", pos.Qty)
		e.attachProtectiveStop(pos)
		fixed++
	}
	return fixed
}

func (e *Engine) attachProtectiveStop(pos broker.HeldPosition) {
	bars, err := e.broker.GetRecentBars(pos.Symbol, e.cfg.Timeframe, e.lookbackDays)
	if err != nil {
		slog.Error("get bars failed while protecting position", "symbol", pos.Symbol, "err", err)
		return
	}
	atr, ok := indicators.ATR(bar.Highs(bars), bar.Lows(bars), bar.Closes(bars), e.params.ATRPeriod)
	if !ok {
		slog.Error("not enough bars to compute ATR for protective stop", "symbol", pos.Symbol, "bars", len(bars))
		return
	}

	price, err := e.broker.GetLatestPrice(pos.Symbol)
	if err != nil {
		slog.Error("get latest price failed while protecting position", "symbol", pos.Symbol, "err", err)
		return
	}

	stopPrice := price - e.params.StopATRMult*atr
	takePrice := price + e.params.TakeATRMult*atr
	if stopPrice <= 0 || stopPrice >= price || takePrice <= price {
		slog.Error("computed invalid protective levels, skipping", "symbol", pos.Symbol, "price", price, "stop", stopPrice, "take", takePrice)
		return
	}

	order, err := e.broker.PlaceProtectiveOCO(pos.Symbol, pos.Qty, stopPrice, takePrice)
	if err != nil {
		slog.Error("failed to attach protective stop", "symbol", pos.Symbol, "err", err)
		return
	}
	slog.Info("protective stop attached", "symbol", pos.Symbol, "qty", pos.Qty, "stop", stopPrice, "take", takePrice, "order_id", order.ID)
}

// logNewFills fetches every fill since the last one recorded and appends
// them to the persistent trade log, logging each one via slog too. This is
// the only place fills get recorded: entries logged at order-submission time
// are intent, not confirmation, and exits (stop-loss/take-profit) happen
// entirely server-side with no log line at all otherwise.
func (e *Engine) logNewFills() {
	since := e.tradeLog.LastTime()
	if since.IsZero() {
		since = time.Now().Add(-24 * time.Hour) // first run: don't backfill ancient history
	}

	fills, err := e.broker.GetFillActivities(since)
	if err != nil {
		slog.Error("get fill activities failed", "err", err)
		return
	}
	if len(fills) == 0 {
		return
	}

	entries := make([]tradelog.Entry, len(fills))
	for i, f := range fills {
		entries[i] = tradelog.Entry{
			ID: f.ID, Time: f.Time, Symbol: f.Symbol, Side: f.Side,
			Qty: f.Qty, Price: f.Price, OrderID: f.OrderID,
		}
		slog.Info("trade fill", "symbol", f.Symbol, "side", f.Side, "qty", f.Qty, "price", f.Price, "order_id", f.OrderID, "time", f.Time)
	}
	if err := e.tradeLog.Append(entries); err != nil {
		slog.Error("failed to persist trade log", "err", err)
	}
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

// rotatingWindow returns up to n symbols starting at offset (wrapping
// around), plus everything left out, so repeated calls with an advancing
// offset eventually cover the whole list instead of always favoring the
// symbols earliest in it.
func rotatingWindow(symbols []string, offset, n int) (window, rest []string) {
	if len(symbols) == 0 {
		return nil, nil
	}
	if n >= len(symbols) {
		return symbols, nil
	}
	offset %= len(symbols)
	inWindow := make(map[int]bool, n)
	for i := 0; i < n; i++ {
		idx := (offset + i) % len(symbols)
		window = append(window, symbols[idx])
		inWindow[idx] = true
	}
	for i, s := range symbols {
		if !inWindow[i] {
			rest = append(rest, s)
		}
	}
	return window, rest
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
