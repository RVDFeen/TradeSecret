// Package backtest simulates the strategy+risk logic in internal/strategy and
// internal/risk over historical bars (daily or intraday), so the approach can
// be sanity checked before it ever touches the (paper) broker.
//
// Simplifications, stated up front rather than hidden:
//   - One fill per bar: entries fill at that bar's open, exits (stop/take/
//     trend-flip) are checked against that bar's high-low-close.
//   - If both the stop and the take-profit fall inside the same bar's
//     high-low range, the stop is assumed to trigger first (conservative).
//   - No commissions or slippage are modeled (Alpaca charges no commission on
//     US equities; slippage on liquid names is small but not zero — real
//     results will differ, more so at intraday timeframes).
//   - The daily loss limit resets on calendar-day boundaries regardless of
//     bar timeframe, matching the live engine's behavior.
package backtest

import (
	"math"
	"sort"
	"time"

	"tradebot/internal/bar"
	"tradebot/internal/risk"
	"tradebot/internal/strategy"
)

type Trade struct {
	Symbol     string
	EntryDate  time.Time
	EntryPrice float64
	ExitDate   time.Time
	ExitPrice  float64
	Qty        int64
	PnL        float64
	Reason     string // "stop", "take_profit", "trend_exit", "end_of_backtest"
}

type EquityPoint struct {
	Date   time.Time
	Equity float64
}

type Result struct {
	StartEquity float64
	EndEquity   float64
	Trades      []Trade
	EquityCurve []EquityPoint
}

type openPosition struct {
	symbol     string
	qty        int64
	entryPrice float64
	entryDate  time.Time
	stopPrice  float64
	takePrice  float64
}

// Run simulates the strategy across symbols -> bars (each already covering
// [start-warmup, end], oldest first) sharing a single cash account of
// startEquity, and returns the combined result.
func Run(symbolBars map[string][]bar.Bar, startEquity float64, rm risk.Manager, params strategy.Params) Result {
	// Bars are keyed by exact timestamp, not calendar day: at intraday
	// timeframes a single day holds many distinct bars.
	timeKey := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }

	dateSet := map[string]time.Time{}
	for _, bars := range symbolBars {
		for _, b := range bars {
			dateSet[timeKey(b.Time)] = b.Time
		}
	}
	dates := make([]time.Time, 0, len(dateSet))
	for _, d := range dateSet {
		dates = append(dates, d)
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })

	// Index bars by symbol+timestamp for O(1) lookup, and keep the full
	// history slice per symbol so we can pass a prefix to strategy.Evaluate.
	barsBySymbol := symbolBars
	idxBySymbol := make(map[string]map[string]int, len(symbolBars))
	for sym, bars := range barsBySymbol {
		m := make(map[string]int, len(bars))
		for i, b := range bars {
			m[timeKey(b.Time)] = i
		}
		idxBySymbol[sym] = m
	}

	symbols := make([]string, 0, len(barsBySymbol))
	for sym := range barsBySymbol {
		symbols = append(symbols, sym)
	}
	sort.Strings(symbols) // deterministic iteration: map order is randomized in Go

	cash := startEquity
	positions := map[string]*openPosition{}
	var trades []Trade
	var curve []EquityPoint

	dayStartEquity := startEquity
	currentCalendarDay := ""
	haltedToday := false

	// lastPrice forward-fills each held symbol's most recently seen close, so
	// a bar missing at this exact timestamp (common across symbols at
	// intraday granularity — not every name trades in every single hour)
	// doesn't get mark-to-market'd as worthless.
	lastPrice := map[string]float64{}

	equityNow := func() float64 {
		total := cash
		for sym, pos := range positions {
			if price, ok := lastPrice[sym]; ok {
				total += float64(pos.qty) * price
			}
		}
		return total
	}

	for _, date := range dates {
		dateKey := timeKey(date)

		// 0) Update the mark-to-market price for every symbol that has a bar
		// at this exact timestamp (others keep their last known price).
		for _, sym := range symbols {
			if idx, ok := idxBySymbol[sym][dateKey]; ok {
				lastPrice[sym] = barsBySymbol[sym][idx].Close
			}
		}

		calendarDay := date.UTC().Format("2006-01-02")
		if calendarDay != currentCalendarDay {
			currentCalendarDay = calendarDay
			dayStartEquity = equityNow()
			haltedToday = false
		}

		// 1) Process exits for existing positions using this bar.
		for sym, pos := range positions {
			idx, ok := idxBySymbol[sym][dateKey]
			if !ok {
				continue
			}
			b := barsBySymbol[sym][idx]

			exitPrice, reason, exited := 0.0, "", false
			if b.Low <= pos.stopPrice {
				exitPrice, reason, exited = pos.stopPrice, "stop", true
			} else if b.High >= pos.takePrice {
				exitPrice, reason, exited = pos.takePrice, "take_profit", true
			} else if idx > 0 && !params.DisableTrendExit {
				// Trend-flip exit: recompute the signal on history up to and
				// including today; if the uptrend has broken, exit at the close.
				if sig, ok := strategy.Evaluate(barsBySymbol[sym][:idx+1], params); ok && !sig.Uptrend {
					exitPrice, reason, exited = b.Close, "trend_exit", true
				}
			}

			if exited {
				pnl := float64(pos.qty) * (exitPrice - pos.entryPrice)
				cash += float64(pos.qty) * exitPrice
				trades = append(trades, Trade{
					Symbol: sym, EntryDate: pos.entryDate, EntryPrice: pos.entryPrice,
					ExitDate: date, ExitPrice: exitPrice, Qty: pos.qty, PnL: pnl, Reason: reason,
				})
				delete(positions, sym)
			}
		}

		// 2) Daily loss kill switch: halt new entries for the rest of the day
		// if the drawdown from today's starting equity already breached the limit.
		currentEquity := equityNow()
		if rm.DailyLossBreached(dayStartEquity, currentEquity) {
			haltedToday = true
		}

		// 3) Process new entries.
		if !haltedToday && rm.CanOpenNewPosition(len(positions)) {
			for _, sym := range symbols {
				bars := barsBySymbol[sym]
				if _, held := positions[sym]; held {
					continue
				}
				if !rm.CanOpenNewPosition(len(positions)) {
					break
				}
				idx, ok := idxBySymbol[sym][dateKey]
				if !ok || idx == 0 {
					continue
				}
				// Signal is computed on bars strictly before this one (up to
				// the previous bar's close) to avoid lookahead bias; the fill
				// happens at this bar's open.
				sig, ok := strategy.Evaluate(bars[:idx], params)
				if !ok || !sig.ShouldEnter {
					continue
				}

				entryPrice := bars[idx].Open
				stopPrice := entryPrice - params.StopATRMult*sig.ATR
				takePrice := entryPrice + params.TakeATRMult*sig.ATR
				equity := equityNow()
				qty := rm.PositionSize(equity, cash, entryPrice, stopPrice)
				if qty <= 0 {
					continue
				}
				cost := float64(qty) * entryPrice
				if cost > cash {
					continue
				}
				cash -= cost
				positions[sym] = &openPosition{
					symbol: sym, qty: qty, entryPrice: entryPrice, entryDate: date,
					stopPrice: stopPrice, takePrice: takePrice,
				}
			}
		}

		curve = append(curve, EquityPoint{Date: date, Equity: equityNow()})
	}

	// Liquidate anything still open at the final available price.
	if len(dates) > 0 {
		lastKey := timeKey(dates[len(dates)-1])
		for sym, pos := range positions {
			idx, ok := idxBySymbol[sym][lastKey]
			if !ok {
				continue
			}
			exitPrice := barsBySymbol[sym][idx].Close
			pnl := float64(pos.qty) * (exitPrice - pos.entryPrice)
			cash += float64(pos.qty) * exitPrice
			trades = append(trades, Trade{
				Symbol: sym, EntryDate: pos.entryDate, EntryPrice: pos.entryPrice,
				ExitDate: dates[len(dates)-1], ExitPrice: exitPrice, Qty: pos.qty, PnL: pnl, Reason: "end_of_backtest",
			})
		}
	}

	end := startEquity
	if len(curve) > 0 {
		end = curve[len(curve)-1].Equity
	}

	return Result{StartEquity: startEquity, EndEquity: end, Trades: trades, EquityCurve: curve}
}

type Stats struct {
	TotalReturnPct float64
	CAGRPct        float64
	MaxDrawdownPct float64
	WinRatePct     float64
	NumTrades      int
	SharpeNaive    float64
}

// Stats summarizes the result. periodsPerYear annualizes the Sharpe ratio and
// should match the bar timeframe the backtest was run on (e.g.
// timeframe.OneDay.PeriodsPerYear() or timeframe.OneHour.PeriodsPerYear()).
func (r Result) Stats(periodsPerYear float64) Stats {
	s := Stats{NumTrades: len(r.Trades)}
	if r.StartEquity <= 0 || len(r.EquityCurve) == 0 {
		return s
	}
	s.TotalReturnPct = (r.EndEquity - r.StartEquity) / r.StartEquity * 100.0

	years := r.EquityCurve[len(r.EquityCurve)-1].Date.Sub(r.EquityCurve[0].Date).Hours() / 24 / 365.25
	if years > 0 && r.EndEquity > 0 {
		s.CAGRPct = (math.Pow(r.EndEquity/r.StartEquity, 1/years) - 1) * 100
	}

	peak := r.EquityCurve[0].Equity
	maxDD := 0.0
	for _, p := range r.EquityCurve {
		if p.Equity > peak {
			peak = p.Equity
		}
		if peak > 0 {
			dd := (peak - p.Equity) / peak * 100
			if dd > maxDD {
				maxDD = dd
			}
		}
	}
	s.MaxDrawdownPct = maxDD

	wins := 0
	for _, t := range r.Trades {
		if t.PnL > 0 {
			wins++
		}
	}
	if len(r.Trades) > 0 {
		s.WinRatePct = float64(wins) / float64(len(r.Trades)) * 100
	}

	// Naive per-bar-return Sharpe (0% risk-free), annualized for the timeframe.
	if len(r.EquityCurve) > 1 {
		rets := make([]float64, 0, len(r.EquityCurve)-1)
		for i := 1; i < len(r.EquityCurve); i++ {
			prev := r.EquityCurve[i-1].Equity
			if prev <= 0 {
				continue
			}
			rets = append(rets, (r.EquityCurve[i].Equity-prev)/prev)
		}
		mean := 0.0
		for _, x := range rets {
			mean += x
		}
		if len(rets) > 0 {
			mean /= float64(len(rets))
			variance := 0.0
			for _, x := range rets {
				variance += (x - mean) * (x - mean)
			}
			variance /= float64(len(rets))
			stddev := math.Sqrt(variance)
			if stddev > 0 {
				s.SharpeNaive = mean / stddev * math.Sqrt(periodsPerYear)
			}
		}
	}

	return s
}
