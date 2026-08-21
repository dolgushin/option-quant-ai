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

## Domain facts (verified live, easy to get wrong)
- **ROPD board mixes two instrument sets under one ASSETCODE** (SBRF/SBPR): share premium options (SHORTNAME like `SBERP160926PE260`, strikes ≈ spot) and options on futures (`SBRF-9.26M…`, strikes ~18000). Always filter by SHORTNAME prefix (`isShareOption` in main.go), never by ASSETCODE alone.
- SBER/SBERP premium option **lot = 100 shares** (multiplier 100; verified via ISS history VALUE/VOLUME ÷ premium). Premium is quoted per one share.
- Share premium options are European, cash-settled on the closing-auction price; expiries are Wednesdays.
- Money units differ: position PnL is rubles (× multiplier × qty), while `spreadRecord.MaxProfit/MaxLoss` and premiums are per-share — always scale via `contractMultiplier(symbol)` before comparing.
- `NetCredit > 0` = credit spread, `< 0` = debit.
- Series lists come from real OPTION expiries (`optionSeriesForSymbol`), codes may be synthetic `"Si-2026-08-20"`. Synthetic codes must be resolved to a tradable future via `resolveRealFuturesCode` wherever a ticker is quoted/hedged (see `getSpotPrice`, `futuresSeriesAlor`).
- Alor Command API v2 endpoints require the unique `X-REQID` header; auth is `POST https://oauth.alor.ru/refresh?token=<refreshToken>` returning `AccessToken` (30 min).

## Conventions
- `KNOWLEDGE.md` is the trading knowledge base; manager defaults and rule semantics reference it by section. Update it together with management-rule changes.
- Commit style: short imperative English ("Add ...", "Fix ...").
- Tests are hermetic where possible: decision logic lives in pure functions (`decideSpreadAction`, `classifyExpiry`) so no network is needed; use `quant.SetDataFile` + temp dirs for store tests.

## Environment gotchas (Windows / PowerShell 5.1)
- **Never rewrite source files through console pipes (`Get-Content | Set-Content`, `Add-Content` here-strings) if they contain non-ASCII text** — the console mangles UTF-8 Cyrillic and corrupts files. Use the Edit tool.
- `$pid` is a reserved automatic variable; pick another name for stored pids.
- PowerShell console prints Cyrillic as mojibake even when data is fine — verify UTF-8 output by writing to a file and reading it, not via console.
- Nested hashtables + `ConvertTo-Json` in PS 5.1 can produce invalid payloads for POST bodies; prefer hand-built JSON strings.
- git warns "LF will be replaced by CRLF" on every commit — normal, ignore.
