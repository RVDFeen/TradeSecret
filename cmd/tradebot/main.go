// Command tradebot runs (or backtests) the trend+momentum paper-trading
// strategy against Alpaca's paper trading API.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"tradebot/internal/backtest"
	"tradebot/internal/bar"
	"tradebot/internal/broker"
	"tradebot/internal/cache"
	"tradebot/internal/config"
	"tradebot/internal/engine"
	"tradebot/internal/risk"
	"tradebot/internal/strategy"
	"tradebot/internal/timeframe"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		runCmd(os.Args[2:])
	case "backtest":
		backtestCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `tradebot — Alpaca paper-trading bot

Usage:
  tradebot run [--once]                     Start the live paper-trading loop
  tradebot backtest [--years N] [--timeframe hourly|daily]
                                             Backtest the strategy over historical data

Configuration is read from .env / environment variables. See .env.example.`)
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	once := fs.Bool("once", false, "evaluate a single pass and exit, instead of looping")
	fs.Parse(args)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config error", "err", err)
		os.Exit(1)
	}

	b := broker.New(cfg)
	acc, err := b.GetAccount()
	if err != nil {
		slog.Error("could not reach Alpaca — check your API keys and network", "err", err)
		os.Exit(1)
	}
	slog.Info("connected to Alpaca paper account", "equity", acc.Equity, "buying_power", acc.BuyingPower, "base_url", cfg.BaseURL)

	eng, err := engine.New(cfg, b)
	if err != nil {
		slog.Error("engine init failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		eng.RunOnce(ctx)
		return
	}
	if err := eng.Run(ctx); err != nil && err != context.Canceled {
		slog.Error("engine stopped", "err", err)
		os.Exit(1)
	}
}

func backtestCmd(args []string) {
	fs := flag.NewFlagSet("backtest", flag.ExitOnError)
	years := fs.Float64("years", 2.0, "how many years of history to backtest over")
	startEquity := fs.Float64("equity", 100000, "starting paper equity for the simulation")
	emaFast := fs.Int("ema-fast", 0, "override EMA fast period (0 = default)")
	emaSlow := fs.Int("ema-slow", 0, "override EMA slow period (0 = default)")
	rsiLow := fs.Float64("rsi-low", 0, "override RSI lower bound (0 = default)")
	rsiHigh := fs.Float64("rsi-high", 0, "override RSI upper bound (0 = default)")
	stopMult := fs.Float64("stop-mult", 0, "override stop-loss ATR multiple (0 = default)")
	takeMult := fs.Float64("take-mult", 0, "override take-profit ATR multiple (0 = default)")
	noTrendExit := fs.Bool("no-trend-exit", false, "disable the early trend-flip exit; rely only on the stop/take bracket")
	timeframeFlag := fs.String("timeframe", "", "bar resolution: \"hourly\" or \"daily\" (default: STRATEGY_TIMEFRAME from .env)")
	fs.Parse(args)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config error", "err", err)
		os.Exit(1)
	}
	tf := cfg.Timeframe
	switch strings.ToLower(*timeframeFlag) {
	case "hourly", "hour":
		tf = timeframe.OneHour
	case "daily", "day":
		tf = timeframe.OneDay
	case "":
		// keep cfg.Timeframe
	default:
		slog.Error("invalid --timeframe, must be \"hourly\" or \"daily\"", "value", *timeframeFlag)
		os.Exit(1)
	}
	b := broker.New(cfg)

	end := time.Now().AddDate(0, 0, -1)
	start := end.AddDate(0, -int(*years*12), 0).AddDate(0, 0, -60) // extra 60d warmup for indicators

	fetch := func(symbol string, start, end time.Time) ([]bar.Bar, error) {
		return b.GetBarsRange(symbol, tf, start, end)
	}
	symbolBars := make(map[string][]bar.Bar, len(cfg.Watchlist))
	for _, sym := range cfg.Watchlist {
		bars, err := cache.GetBars(sym, tf.String(), start, end, fetch)
		if err != nil {
			slog.Error("fetching history failed", "symbol", sym, "err", err)
			os.Exit(1)
		}
		slog.Info("loaded history", "symbol", sym, "bars", len(bars))
		symbolBars[sym] = bars
	}

	rm := risk.Manager{
		RiskPerTradePct:   cfg.RiskPerTradePct,
		MaxPositionPct:    cfg.MaxPositionPct,
		MaxPositions:      cfg.MaxPositions,
		DailyLossLimitPct: cfg.DailyLossLimitPct,
	}
	var params strategy.Params
	if tf.Unit == timeframe.Hour {
		params = strategy.DefaultHourlyParams()
	} else {
		params = strategy.DefaultDailyParams()
	}
	if *emaFast > 0 {
		params.EMAFastPeriod = *emaFast
	}
	if *emaSlow > 0 {
		params.EMASlowPeriod = *emaSlow
	}
	if *rsiLow > 0 {
		params.RSILowerBound = *rsiLow
	}
	if *rsiHigh > 0 {
		params.RSIUpperBound = *rsiHigh
	}
	if *stopMult > 0 {
		params.StopATRMult = *stopMult
	}
	if *takeMult > 0 {
		params.TakeATRMult = *takeMult
	}
	if *noTrendExit {
		params.DisableTrendExit = true
	}

	result := backtest.Run(symbolBars, *startEquity, rm, params)
	stats := result.Stats(tf.PeriodsPerYear())

	fmt.Printf("\n=== Backtest report (%s to %s, %s bars) ===\n", start.Format("2006-01-02"), end.Format("2006-01-02"), tf)
	fmt.Printf("Symbols:            %v\n", cfg.Watchlist)
	fmt.Printf("Start equity:       $%.2f\n", result.StartEquity)
	fmt.Printf("End equity:         $%.2f\n", result.EndEquity)
	fmt.Printf("Total return:       %.2f%%\n", stats.TotalReturnPct)
	fmt.Printf("CAGR:               %.2f%%\n", stats.CAGRPct)
	fmt.Printf("Max drawdown:       %.2f%%\n", stats.MaxDrawdownPct)
	fmt.Printf("Trades:             %d\n", stats.NumTrades)
	fmt.Printf("Win rate:           %.1f%%\n", stats.WinRatePct)
	fmt.Printf("Naive Sharpe:       %.2f\n", stats.SharpeNaive)
	fmt.Println()
	fmt.Println("NOTE: past performance on historical data does not predict future paper (or real) results.")
	fmt.Println("This ignores slippage/commissions and uses a simplified same-day stop/take model — treat as directional, not exact.")
}
