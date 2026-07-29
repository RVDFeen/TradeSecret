# tradebot

A long-only, trend + momentum swing-trading bot for Alpaca **paper trading**,
written in Go. It trades daily bars: enter when a name is in a confirmed
uptrend with healthy (not overbought) momentum, size the position by
volatility so every trade risks a fixed % of equity, and protect every
position with a broker-side stop-loss/take-profit bracket so the exit is
enforced by Alpaca even if the bot itself is offline.

**Read this before expecting anything from it:** no strategy "maximizes
profit" — that's not a real property a trading system can have. What's here
is a reasonably engineered, risk-managed system whose default parameters were
chosen by walk-forward backtesting (staying net-positive with a decent Sharpe
ratio across five different lookback windows, 1.5–5 years), not by picking the
single best result on one period. Treat the backtest numbers as directional
evidence of a modest edge, not a guarantee — see [Honest limitations](#honest-limitations).

## Safety rails already built in

- **Refuses to run against anything but the paper endpoint.** `config.Load()`
  hard-fails unless `APCA_API_BASE_URL` contains `paper-api`. Live trading
  would need a deliberate code change, not a config typo.
- **Every entry is a bracket order** (stop-loss + take-profit attached
  server-side at submission), so a stop is never "forgotten."
- **Daily loss kill switch**: if equity drawdown from the day's start exceeds
  `DAILY_LOSS_LIMIT_PCT`, the bot cancels open orders and stops opening new
  positions for the rest of the day.
- **Position sizing by risk, not gut feel**: each trade risks
  `RISK_PER_TRADE_PCT` of equity (based on the ATR-derived stop distance),
  capped by `MAX_POSITION_PCT` of equity and by `MAX_POSITIONS` concurrent names.

## Strategy

Evaluated on daily bars per symbol:

- **Trend filter**: EMA(20) > EMA(50) and price > EMA(50).
- **Momentum filter**: RSI(14) between 40 and 70 (healthy, not overbought/oversold).
- **Entry**: both filters true, no existing position/order in that symbol, and
  a free slot under `MAX_POSITIONS`.
- **Stop-loss**: entry − 2.5×ATR(14). **Take-profit**: entry + 4×ATR(14).
- **Exit**: whichever bracket leg fills first. (An earlier "trend flip" exit
  was tested and backtested *worse* — it clips winners before they reach the
  target — so it's disabled by default; see `--no-trend-exit` below.)

Long-only by design: shorting isn't wired up. Easy to add later, but it
roughly doubles the ways this can go wrong before you've validated the long
side works.

## Setup

```
go build -o bin/tradebot ./cmd/tradebot
```

Credentials and config live in `.env` (already populated from what you gave
me — **rotate that key if it's ever been shared anywhere else**, since it's
sitting in plaintext on disk). `.env` is gitignored; `.env.example` shows the
shape without secrets.

## Usage

```
# Backtest the current default parameters over N years of history
./bin/tradebot backtest --years 3

# Try different parameters without touching code
./bin/tradebot backtest --years 3 --ema-fast 20 --ema-slow 50 \
  --stop-mult 2.5 --take-mult 4 --no-trend-exit

# One evaluation pass against the live paper account, then exit
./bin/tradebot run --once

# The real loop: polls every POLL_INTERVAL while the market is open
./bin/tradebot run
```

Historical bars are cached under `.cache/<symbol>.json` so repeated backtest
runs (parameter sweeps) don't re-hit Alpaca's market data API — the second run
of an identical backtest command took ~30ms instead of ~2.5s in testing.
Delete `.cache/` if you want a hard refresh.

## Backtest results (as of this build, 9-symbol default watchlist)

| Window | Total return | CAGR | Max drawdown | Trades | Win rate | Sharpe (naive) |
|---|---|---|---|---|---|---|
| 1.5y | 7.0% | 4.2% | 11.2% | 76 | 43.4% | 0.41 |
| 2y | 10.9% | 4.9% | 11.3% | 109 | 43.1% | 0.44 |
| 3y | 27.9% | 8.1% | 15.4% | 176 | 44.9% | 0.62 |
| 4y | 49.6% | 10.2% | 15.5% | 241 | 46.1% | 0.74 |
| 5y | 43.1% | 7.2% | 27.6% | 285 | 44.2% | 0.57 |

Re-run `./bin/tradebot backtest --years N` any time to reproduce — it's
deterministic given the same cached data.

## Honest limitations

- **This backtest window is mostly one long bull market.** A long-only trend
  strategy looking good over 2023–2026 is not surprising and is not strong
  evidence it'll hold up in a sustained downtrend or a choppy range — there's
  no bear-market data in this sample to test against.
- **No slippage or commissions modeled.** Alpaca doesn't charge commission on
  US equities, but the backtest still assumes you get exactly the stop/limit
  price on the day it's crossed, which is optimistic.
- **Same-day stop/take ambiguity**: if a day's high and low both cross the
  take-profit and stop, the backtest assumes the stop hit first (conservative
  assumption, but still an assumption).
- **Small sample.** A few hundred trades across 9 correlated, mostly
  large-cap-tech-and-index symbols is not a lot of independent evidence.
  Parameters were tuned on this same data/watchlist, so there's real overfitting
  risk baked in even though I checked robustness across multiple windows
  rather than optimizing a single one.
- **Daily-bar granularity** means it's a swing strategy, not a day-trading one
  — it will hold names overnight and over weekends, with the accompanying gap
  risk, which the ATR stop does not protect against (a gap can blow through it).

None of this means "don't run it" — it means: watch it in paper for a while,
don't mistake a backtest curve for a promise, and don't be surprised if live
paper results diverge from the table above.

## Project layout

```
cmd/tradebot/          CLI entrypoint (run / backtest subcommands)
internal/config/       .env + environment variable loading
internal/broker/       Alpaca trading + market-data API wrapper
internal/bar/          broker-agnostic OHLCV bar type
internal/indicators/   EMA, RSI, ATR, SMA
internal/strategy/     entry/exit signal logic
internal/risk/         position sizing, daily loss limit, exposure caps
internal/engine/       live polling loop tying broker+strategy+risk together
internal/backtest/     historical simulation + performance stats
internal/cache/        on-disk cache for historical bars
```
