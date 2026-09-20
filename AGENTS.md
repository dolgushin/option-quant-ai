# AGENTS.md — option-quant-ai

## Commands
- Everything lives in `backend/` (Go module `option-quant-ai`, go 1.22). Run from there:
  - Build/vet/test: `go build ./... && go vet ./... && go test ./...`
  - Force fresh tests (package cache): `go test -count=1 .`
- Run server locally: env `PORT` (default 9000) and `DATA_DIR` (JSON stores + encrypted token store are created here).
- Smoke-test pattern used throughout the session: build exe to `%TEMP%\opencode\oq-test\`, `Start-Process` with PORT/DATA_DIR, probe with `Invoke-RestMethod`, save pid to file, `Stop-Process`. Kill leftover servers before finishing.

## Architecture
- Single Go binary serves the SPA at `backend/static/index.html` (Tailwind CDN classes, vanilla JS, Chart.js hosted locally). No frontend build step.
- Packages: `quant` (portfolio/BS greeks/persistence), `alor` (auth/market/exec), `secure` (encrypted token store), root package = HTTP handlers.
- Market data comes from public MOEX ISS (`iss.moex.com/iss/engines/futures/markets/...`) with in-process caches (~10 min TTL); Alor API is the primary live feed for books/quotes/spot plus order execution when a refresh token is saved via `/api/v1/settings/token`.
- Trade universe is Si + RI only (`coreSymbols`, brief symbols, UI selects trimmed); other symbols' code paths and trade history are untouched.
- Vertical spreads: `spreads.go` (records/builders/handlers), `spreads_manager.go` (auto-manager loop every 60s + state machine), rules via `POST /api/v1/spreads/rules`, log via `GET /api/v1/spreads/manager`.
- Core candidates (`core_data.go`): economics sanity (`planEconomicsSane`) → quality floor (`planMeetsQualityFloor`, credit ≥10% wing) → twin check → score (KB 0–100 + PoP ±8 + ATR adjust, RAW — weights never scale it) → gate 45. Skip reasons ride in `brief.Skipped`.
- Short straddles: `straddle.go` (classic/covered/synthetic builders, 4 hedge rules via `decideStraddleHedge` with cover-aware target, 60s loop with stops, manual hedge button, profile with theta accrual + hedge forecast + leg quote provenance).
- Futures legs are marked at their own contract (never the selected-series spot); analytics uses the futures mark as curve spot with a suspect flag.
- Hedge fills are executable book touches only; opposite futures legs net FIFO via `quant.NetFuturesLegs` with realized P&L preserved and journaled once via `quant.SettleTrade` (all close sites including roll intermediates).
- All automation loops halt on weekends MSK (`weekendHalt`; crypto exempt).
- No estimate prices anywhere: builders refuse, analytics flags, MC asks for manual input, managers stand down price triggers on estimate spots.
- ML scan (`ml_module.go`, `POST /api/v2/ml/scan`) and MC grid scan (`mc_pnl.go`, `POST /api/v1/mc-scan`, fixed seed 42).
- Telegram alerts carry payoff PNGs (`spread_chart.go`, stdlib + `golang.org/x/image` basicfont, light theme); dedup keys persist in `core_state.json` and are marked only after successful send; send failures log as `telegram: … failed:`.
- Pre-trade decision panel: `spread_advice.go` (`GET /api/v1/spreads/advice`, weighted 0–100 score).
- MOEX-constructor analytics: `spread_analytics.go` (`GET /api/v1/spreads/analytics?id=…` or plan params) — P&L now (BS at per-leg IV) vs expiry curves, delta/theta curves, per-leg greeks + totals.
- Statistics module: `stats_module.go` (`/api/v2/stats/{overview,breakdown}`) — pure aggregators `computeStatsOverview` / `computeBreakdown`.
- Forecast module: `forecast_module.go` (`/api/v2/forecast`) — bootstrap Monte-Carlo (`mcFan`, seeded rand for determinism), per-strategy t-stats, regime advice.
- Trade journal: every closed trade is enriched by `enrichTradeContext` (main.go) with DTE/entry spot/historical ATM IV/trend/vol regime at entry — stats and forecast bucket on these fields; old trades have them empty ("нет данных").
- Dashboard lists (`/api/v1/positions`, `/api/v1/trades`) hide spread-linked entries (`isSpreadPositionID`, `isSpreadTrade`); spreads live on the Spreads tab, stats count everything.

## Domain facts (verified live, easy to get wrong)
- **ROPD board mixes two instrument sets under one ASSETCODE** (SBRF/SBPR): share premium options (SHORTNAME like `SBERP160926PE260`, strikes ≈ spot) and options on futures (`SBRF-9.26M…`, strikes ~18000). Always filter by SHORTNAME prefix (`isShareOption` in main.go), never by ASSETCODE alone.
- SBER/SBERP premium option **lot = 100 shares** (multiplier 100; verified via ISS history VALUE/VOLUME ÷ premium). Premium is quoted per one share.
- Share premium options are European, cash-settled on the closing-auction price; expiries are Wednesdays.
- Money units differ: position PnL is rubles (× multiplier × qty), while `spreadRecord.MaxProfit/MaxLoss` and premiums are per-share — always scale via `contractMultiplier(symbol)` before comparing.
- `NetCredit > 0` = credit spread, `< 0` = debit.
- Series lists come from real OPTION expiries (`optionSeriesForSymbol`), codes may be synthetic `"Si-2026-08-20"`. Synthetic codes must be resolved to a tradable future via `resolveRealFuturesCode` wherever a ticker is quoted/hedged (see `getSpotPrice`, `futuresSeriesAlor`).
- Alor Command API v2 endpoints require the unique `X-REQID` header; auth is `POST https://oauth.alor.ru/refresh?token=<refreshToken>` returning `AccessToken` (30 min).
- **Option marks are hybrid** (`optionMark`/`optionMarkWithSrc` in option_mark.go): live two-sided books with spread ≤ 25% of mid (`quoteIsLive`) are marked at mid; dead/wide/stale books are marked at Black-Scholes fair value using the series IV (`seriesIVForExpiry`, median of liquid near-ATM strikes, fallback realized/0.30). The official MOEX constructor prices illiquid series the same way — mid marks on dead books are noise (a 500-wide spread must never "move to" 2400). `mark_src` in leg JSON shows `mid|last|theo|none`.
- **Telegram goes through a RU relay, not api.telegram.org**: `telegram.go` uses `telegramAPIBase = "http://193.233.87.23/bot8627553310"` — never flip back to `https://api.telegram.org/bot<TOKEN>` (timeout from RF VPS). Event-only messages (no digest, no verdict heartbeat): DTE≤5 expiry alerts, candidate charts, paper auto-entries, structure closes, manager early warnings (70% zone, 6h cooldown) and action receipts. `getUpdates` for chat_id discovery also runs on the relay.

## Conventions
- `KNOWLEDGE.md` is the trading knowledge base; manager defaults and rule semantics reference it by section. Update it together with management-rule changes. `README.md` is the user-facing overview (features, quick start, API map) — keep both in sync when adding modules.
- Commit style: short imperative English ("Add ...", "Fix ...").
- Tests are hermetic where possible: decision logic lives in pure functions (`decideSpreadAction`, `classifyExpiry`, `computeStatsOverview`, `mcFan`, `scoreSpreadAdvice`, `buildSpreadAnalytics`, `planEconomicsSane`, `planMeetsQualityFloor`, `popScoreAdjust`, `scanMLCombinations`, `simulateSpreadPnL`, `decideStraddleHedge`, `hedgeTargetDelta`, `hedgeNotifyWanted`, `profileRange`, `managerEarlyWarnings`, `applyLotCap`, `encodeStrategyName`, `tenorOf`, `analyticsSpot`, `tradingHalted`, `parseStrikesFromSymbols`, `buildStrikeOptions`) so no network is needed; use `quant.SetDataFile` + temp dirs for store tests.
- The module is no longer stdlib-only: `golang.org/x/image` (basicfont) is used for Telegram payoff-chart labels. Fresh clones need network once for `go mod download`; after that builds/tests are offline via the module cache.

## Environment gotchas (Windows / PowerShell 5.1)
- **Never rewrite source files through console pipes (`Get-Content | Set-Content`, `Add-Content` here-strings) if they contain non-ASCII text** — the console mangles UTF-8 Cyrillic and corrupts files. Use the Edit tool.
- `$pid` is a reserved automatic variable; pick another name for stored pids.
- PowerShell console prints Cyrillic as mojibake even when data is fine — verify UTF-8 output by writing to a file and reading it, not via console.
- Nested hashtables + `ConvertTo-Json` in PS 5.1 can produce invalid payloads for POST bodies; prefer hand-built JSON strings.
- git warns "LF will be replaced by CRLF" on every commit — normal, ignore.
