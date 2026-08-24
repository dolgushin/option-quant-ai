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
- Market data comes from public MOEX ISS (`iss.moex.com/iss/engines/futures/markets/...`) with in-process caches (~10 min TTL); Alor API is only for live orders/quotes when a refresh token is saved via `/api/v1/settings/token`.
- Vertical spreads: `spreads.go` (records/builders/handlers), `spreads_manager.go` (auto-manager loop every 60s + state machine), rules via `POST /api/v1/spreads/rules`, log via `GET /api/v1/spreads/manager`.
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

## Conventions
- `KNOWLEDGE.md` is the trading knowledge base; manager defaults and rule semantics reference it by section. Update it together with management-rule changes. `README.md` is the user-facing overview (features, quick start, API map) — keep both in sync when adding modules.
- Commit style: short imperative English ("Add ...", "Fix ...").
- Tests are hermetic where possible: decision logic lives in pure functions (`decideSpreadAction`, `classifyExpiry`, `computeStatsOverview`, `mcFan`, `scoreSpreadAdvice`, `buildSpreadAnalytics`) so no network is needed; use `quant.SetDataFile` + temp dirs for store tests.

## Environment gotchas (Windows / PowerShell 5.1)
- **Never rewrite source files through console pipes (`Get-Content | Set-Content`, `Add-Content` here-strings) if they contain non-ASCII text** — the console mangles UTF-8 Cyrillic and corrupts files. Use the Edit tool.
- `$pid` is a reserved automatic variable; pick another name for stored pids.
- PowerShell console prints Cyrillic as mojibake even when data is fine — verify UTF-8 output by writing to a file and reading it, not via console.
- Nested hashtables + `ConvertTo-Json` in PS 5.1 can produce invalid payloads for POST bodies; prefer hand-built JSON strings.
- git warns "LF will be replaced by CRLF" on every commit — normal, ignore.
