# tradebot

A long-only, trend + momentum trading bot for Alpaca **paper trading**,
written in Go. It runs on **hourly bars** by default (a faster, more
day-trading-flavored setup than a daily swing strategy — configurable back to
daily via `STRATEGY_TIMEFRAME`): enter when a name is in a confirmed uptrend
with healthy (not overbought) momentum, size the position by volatility so
every trade risks a fixed % of equity, and protect every position with a
broker-side stop-loss/take-profit order so the exit is enforced by Alpaca
even if the bot itself is offline.

**Read this before expecting anything from it:** no strategy "maximizes
profit" or gets you "money fast" — those aren't real properties a trading
system can have. Faster trading (more trades/week) means more slippage
exposure and more chances to be wrong before an edge plays out, not a
shortcut to bigger returns; the risk management here (fixed risk-per-trade,
stops on every position) is not loosened just because the timeframe got
faster. What's here is a reasonably engineered, risk-managed system whose
default parameters were chosen by walk-forward backtesting (staying
net-positive with a decent Sharpe ratio across several lookback windows), not
by picking the single best result on one period. Treat the backtest numbers
as directional evidence of a modest edge, not a guarantee — see
[Honest limitations](#honest-limitations).

## Safety rails already built in

- **Refuses to run against anything but the paper endpoint.** `config.Load()`
  hard-fails unless `APCA_API_BASE_URL` contains `paper-api`. Live trading
  would need a deliberate code change, not a config typo.
- **Every entry is a bracket order** (stop-loss + take-profit attached
  server-side at submission), so a stop is never "forgotten."
- **Every tick, every held position is checked for a live protective stop —
  not just ones this bot opened.** If a position exists without one (opened
  manually, or a bracket leg got cancelled some other way), the bot computes
  an ATR-based stop/take from current price and attaches it immediately via a
  standalone OCO order. Verified against the paper account: manually stripped
  a position's bracket, confirmed the next tick detected it and re-protected
  it within one poll cycle.
- **Daily loss kill switch**: if equity drawdown from the day's start exceeds
  `DAILY_LOSS_LIMIT_PCT`, the bot cancels open orders and stops opening new
  positions for the rest of the day — then immediately re-attaches protective
  stops to existing positions (cancel-all also strips those, which the
  guardian above corrects right away instead of leaving positions naked until
  the next poll).
- **Position sizing is calculated, not fixed.** Size = however many shares
  keep the loss at the stop to `RISK_PER_TRADE_PCT` of equity — a tighter
  ATR-based stop sizes bigger, a wider one sizes smaller, entirely from that
  math. `MAX_POSITION_PCT` and buying power are backstops, not targets: the
  former only catches degenerate cases (e.g. a near-zero ATR), the latter is
  just "can't spend money you don't have." Neither is meant to be the thing
  that binds day to day — if it is, `RISK_PER_TRADE_PCT` or the stop distance
  is the actual lever to revisit, not the cap.
- **No state survives a restart, by design.** Every tick re-fetches equity,
  cash, buying power, and every open position straight from Alpaca — nothing
  is cached across runs. A crash or power cut can't cause "phantom" trading
  behavior on restart, because there's no in-memory state to be wrong about;
  it always acts on whatever Alpaca says is true right now.

## Strategy

Timeframe-agnostic logic, with separate tuned parameters per timeframe since
"EMA(20)" means a very different thing on hourly vs. daily bars:

|  | Hourly (default) | Daily |
|---|---|---|
| EMA fast / slow | 9 / 21 | 20 / 50 |
| RSI band | 40–70 | 40–70 |
| Stop / take (×ATR) | 2.0 / 3.0 | 2.5 / 4.0 |

- **Trend filter**: EMA(fast) > EMA(slow) and price > EMA(slow).
- **Momentum filter**: RSI(14) within the band above (healthy, not overbought/oversold).
- **Entry**: both filters true, no existing position/order in that symbol, and
  a free slot under `MAX_POSITIONS`.
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

## Backtest results (as of this build, hourly bars, 9-symbol default watchlist)

`./bin/tradebot backtest --years N --timeframe hourly`, with `MAX_POSITION_PCT`
as a backstop (100%) rather than a binding cap — i.e. the numbers below
reflect genuinely risk-calculated sizing, not a fixed % per trade:

| Window | Total return | CAGR | Max drawdown | Trades | Win rate | Sharpe (naive) |
|---|---|---|---|---|---|---|
| 0.5y | 39.8% | 66.8% | 8.7% | 281 | 46.3% | 2.08 |
| 1y | 31.6% | 26.6% | 17.5% | 544 | 44.1% | 1.03 |
| 1.5y | 22.2% | 12.8% | 25.7% | 783 | 43.8% | 0.56 |
| 2y | 41.3% | 17.4% | 23.7% | 961 | 43.1% | 0.73 |

Notice the drawdowns are meaningfully higher than a capped-sizing version
would show (was 9–20% under a 20% cap) — that's the direct, expected
consequence of letting position size follow the risk math instead of an
arbitrary ceiling: bigger, more genuinely-sized positions raise both return
and drawdown together. If that trade-off isn't what you want, `MAX_POSITION_PCT`
is the honest lever — set it to an actual concentration limit you intend to
respect, not a number that happens to look reasonable.

Re-run `./bin/tradebot backtest --years N` any time to reproduce — it's
deterministic given the same cached data. `--timeframe daily` runs the
slower swing-trading variant instead (see the strategy table above).

## Honest limitations

- **This backtest window is mostly one long bull market**, and Alpaca's free
  IEX feed only has ~2 years of hourly history available to test against
  regardless. A long-only trend strategy looking good here is not surprising
  and is not strong evidence it'll hold up in a sustained downtrend or a
  choppy range — there's no bear-market data in this sample at all.
- **No slippage or commissions modeled.** Alpaca doesn't charge commission on
  US equities, but the backtest still assumes you get exactly the stop/limit
  price the instant it's crossed, which is more optimistic at hourly
  granularity than daily (faster-moving bars, tighter stops).
- **Same-bar stop/take ambiguity**: if one bar's high and low both cross the
  take-profit and stop, the backtest assumes the stop hit first (conservative
  assumption, but still an assumption).
- **Small sample, more so than it looks.** Hundreds to low thousands of trades
  across 9 correlated, mostly large-cap-tech-and-index symbols is not nearly
  as much independent evidence as the trade count suggests — correlated names
  mean correlated trades. Parameters were tuned on this same data/watchlist,
  so there's real overfitting risk even though robustness was checked across
  multiple windows rather than optimizing a single one.
- **Uncapped-ish sizing raises real drawdown, not just backtest drawdown.**
  Since `MAX_POSITION_PCT` is now a backstop rather than a binding cap, a
  string of losses can compound faster than a tightly-capped version would.
  The daily loss kill switch is the actual brake on that, not the position cap.

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
