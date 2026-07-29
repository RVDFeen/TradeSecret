// Package config loads bot configuration from a .env file and environment variables.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"tradebot/internal/ratelimit"
	"tradebot/internal/timeframe"
)

type Config struct {
	APIKeyID     string
	APISecret    string
	BaseURL      string
	Watchlist    []string
	PollInterval time.Duration
	Timeframe    timeframe.Timeframe // bar resolution the strategy trades on

	// DynamicUniverse, when true, ignores Watchlist for the live engine and
	// instead trades whichever UniverseSize symbols ranked most liquid in
	// today's daily scan across (almost) every tradable US equity. Watchlist
	// remains what `backtest` uses either way — the dynamic universe is a
	// live-only feature (see README for why).
	DynamicUniverse bool
	UniverseSize    int

	RiskPerTradePct   float64 // % of equity risked per trade (via stop distance)
	MaxPositionPct    float64 // max % of equity in a single position
	MaxPositions      int     // max number of concurrent open positions
	DailyLossLimitPct float64 // halt new entries / flatten if equity drawdown exceeds this in a day
}

// loadDotEnv reads KEY=VALUE pairs from path into the process environment,
// without overriding variables already set in the real environment.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
	return scanner.Err()
}

func getFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func getInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return i
}

// defaultPollInterval picks a sensible poll cadence for a timeframe: fast
// enough to notice a new bar promptly, without polling far more often than
// the signal itself can possibly change.
func defaultPollInterval(tf timeframe.Timeframe) time.Duration {
	switch tf.Unit {
	case timeframe.Minute:
		return 30 * time.Second
	default: // Hour, Day
		return 15 * time.Minute
	}
}

// Load reads .env (if present) and environment variables into a Config.
func Load() (*Config, error) {
	_ = loadDotEnv(".env")

	cfg := &Config{
		APIKeyID:  os.Getenv("APCA_API_KEY_ID"),
		APISecret: os.Getenv("APCA_API_SECRET_KEY"),
		BaseURL:   os.Getenv("APCA_API_BASE_URL"),
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://paper-api.alpaca.markets"
	}
	if cfg.APIKeyID == "" || cfg.APISecret == "" {
		return nil, fmt.Errorf("APCA_API_KEY_ID and APCA_API_SECRET_KEY must be set (via .env or environment)")
	}

	watchlistRaw := os.Getenv("WATCHLIST")
	if watchlistRaw == "" {
		watchlistRaw = "AAPL,MSFT,NVDA,AMZN,GOOGL,META,TSLA,SPY,QQQ"
	}
	for _, s := range strings.Split(watchlistRaw, ",") {
		s = strings.TrimSpace(strings.ToUpper(s))
		if s != "" {
			cfg.Watchlist = append(cfg.Watchlist, s)
		}
	}

	switch strings.ToLower(strings.TrimSpace(os.Getenv("STRATEGY_TIMEFRAME"))) {
	case "", "hourly", "hour":
		cfg.Timeframe = timeframe.OneHour
	case "daily", "day":
		cfg.Timeframe = timeframe.OneDay
	case "minute", "min", "1min":
		cfg.Timeframe = timeframe.OneMinute
	default:
		return nil, fmt.Errorf("invalid STRATEGY_TIMEFRAME %q: must be \"minute\", \"hourly\", or \"daily\"", os.Getenv("STRATEGY_TIMEFRAME"))
	}

	// POLL_INTERVAL's default tracks the timeframe (so switching timeframe
	// doesn't silently leave polling mismatched to it) but an explicit
	// POLL_INTERVAL always wins, at any timeframe.
	pollRaw := os.Getenv("POLL_INTERVAL")
	if pollRaw == "" {
		pollRaw = defaultPollInterval(cfg.Timeframe).String()
	}
	d, err := time.ParseDuration(pollRaw)
	if err != nil {
		return nil, fmt.Errorf("invalid POLL_INTERVAL %q: %w", pollRaw, err)
	}
	cfg.PollInterval = d

	cfg.DynamicUniverse = strings.EqualFold(strings.TrimSpace(os.Getenv("UNIVERSE_MODE")), "dynamic")
	cfg.UniverseSize = getInt("UNIVERSE_SIZE", 50)

	cfg.RiskPerTradePct = getFloat("RISK_PER_TRADE_PCT", 1.0)
	cfg.MaxPositionPct = getFloat("MAX_POSITION_PCT", 100.0)
	cfg.MaxPositions = getInt("MAX_POSITIONS", 5)
	cfg.DailyLossLimitPct = getFloat("DAILY_LOSS_LIMIT_PCT", 3.0)

	// Hard safety floor: position-protection and account-state checks are
	// non-negotiable every tick, so POLL_INTERVAL can never go fast enough to
	// risk not being able to afford them. This floor doesn't depend on
	// MAX_POSITIONS or universe size — those calls are single batched
	// requests regardless of how many positions/symbols they cover.
	// Candidate scanning (which does scale with universe size) is throttled
	// separately in the engine instead of by slowing this down further —
	// see internal/ratelimit.
	if floor := ratelimit.MinPollInterval(); cfg.PollInterval < floor {
		fmt.Fprintf(os.Stderr, "WARNING: POLL_INTERVAL %s is below the rate-limit safety floor (%s) — raising it to the floor.\n",
			cfg.PollInterval, floor)
		cfg.PollInterval = floor
	}

	if !strings.Contains(cfg.BaseURL, "paper-api") {
		fmt.Fprintln(os.Stderr, "WARNING: APCA_API_BASE_URL does not look like the paper trading endpoint. Refusing to run against a live-money endpoint from this bot.")
		return nil, fmt.Errorf("refusing to run: base URL %q is not the paper trading endpoint", cfg.BaseURL)
	}

	return cfg, nil
}
