# oteldb cross-engine benchmark

A reproducible, docker-compose-based harness that benchmarks
[`oteldb`](../oteldb) against the other observability engines on **one dataset,
one set of queries, one machine** — across all three signals and their query
languages:

| Signal  | Reference language | Systems under test |
|---------|--------------------|--------------------|
| Metrics | **PromQL**  | oteldb · oteldb-ch · Prometheus · VictoriaMetrics · Mimir · GreptimeDB · gigapipe |
| Logs    | **LogQL**   | oteldb · oteldb-ch · Loki · VictoriaLogs\* · gigapipe |
| Traces  | **TraceQL** | oteldb · oteldb-ch · Tempo · VictoriaTraces\* · gigapipe |

**oteldb** is benched in two configurations from the *same binary*: `oteldb` is
the embedded go-faster/storage engine (`--embedded`, `file` backend), and
`oteldb-ch` serves the same three query APIs from oteldb's original ClickHouse
storage (chstorage) on a dedicated `clickhouse-oteldb` server. Identical query
semantics and ingest path — the comparison isolates the storage layer.

\* VictoriaLogs and VictoriaTraces do not implement LogQL/TraceQL; they are
benched on their native dialect (LogsQL / Jaeger query) over the *same dataset
and hardware*. Those rows are marked `*` in the report — comparable on
data/ingest/footprint, **not** on query-language semantics.

The whole matrix is encoded in [`systems.yml`](systems.yml) — the single source
of truth every script reads.

---

## The idea in four moves

The design rests on four properties of this ecosystem that make a fair
apples-to-apples comparison possible:

1. **One shared object store.** [`go-faster/fs`](https://github.com/go-faster/fs)
   runs as a single S3 server (`compose/base.yml`, service `fs`). Every
   object-store-native engine — oteldb, Mimir, Loki, Tempo, GreptimeDB — is
   pointed at it, one bucket each (`fs-init` creates them). So the *durability
   substrate is identical* for that group; differences in on-disk size are
   differences in the engines' columnar formats and compression, not in the
   storage layer. Engines that are local-disk-only (Prometheus, VictoriaMetrics,
   VictoriaLogs, VictoriaTraces) and ClickHouse-backed (gigapipe) use their
   native storage and are labelled accordingly.

2. **One source, fanned out.** Each lane drives one generator that reaches every
   backend identically. **Metrics:** a single `node-exporter` is scraped by
   `vmagent` as 10 synthetic hosts (`config/vmagent/scrape.yml`) and remote-written
   (standard snappy) to all six engines live — current timestamps, in order, so
   there are no static-dataset timestamp problems. **Logs/traces:** one
   OpenTelemetry Collector (`config/otelcol/<lane>.yml`) mirrors the OTLP stream to
   every backend. Every engine therefore ingests *the same data*.

3. **One query driver, many endpoints.** Every engine here speaks a
   *compatible HTTP query API* for its reference language (Prometheus
   `/api/v1`, Loki `/loki/api/v1`, Tempo `/api/search`). The `benchctl query`
   command replays the same query suite (`queries/*.yml`) against each system's
   `addr` from `systems.yml` and records latency percentiles. Native-dialect
   systems get an id-matched override from `queries/native/`.

4. **One footprint probe.** `benchctl collect` reads each system's on-disk
   size (its bucket in `fs` and/or its named volume) and live RSS, so the report
   pairs **query latency** with **storage + memory cost**.

```
                         canonical dataset (once)
                                  │
                    ┌─────────────┴──────────────┐
                    │   OTel Collector (fan-out)  │
                    └──┬───┬───┬───┬───┬──────────┘
   remote-write / OTLP │   │   │   │   │  (identical data to every backend)
        ┌──────────────┘   │   │   │   └─────────────┐
     oteldb            prometheus … mimir …       gigapipe
        │                  │          │               │
        └── go-faster/fs S3 (shared bucket server) ───┘   ← apples-to-apples substrate
                    ▲
   benchctl query   replays queries/*.yml over each system's HTTP query API
   benchctl collect measures bucket/volume size + RSS
   benchctl report  → results/REPORT.md
```

---

## Reproduce

Requirements: `docker` (+ compose v2) and **Go** (≥1.24). Everything is driven by
the `benchctl` Go CLI (`cmd/benchctl`) — no bash, `yq` or `awk` needed. Go also
runs the repo's `otelbench` (from `../oteldb`) for metrics/logs ingest; trace
ingest uses `telemetrygen` in a container.

```bash
cp .env.example .env            # tweak CPU/mem caps, images, creds

# One lane end-to-end (up → ingest → settle → query → footprint):
go run ./cmd/benchctl bench metrics
go run ./cmd/benchctl bench logs
go run ./cmd/benchctl bench traces

# …or the whole matrix + merged report:
go run ./cmd/benchctl bench-all     # writes results/REPORT.md

go run ./cmd/benchctl down          # stop + wipe volumes
```

Prefer a binary or the Task wrapper:

```bash
go build -o benchctl ./cmd/benchctl && ./benchctl bench-all
# or:  task bench:all      task bench LANE=metrics      task down

# Individual steps, à la carte:
./benchctl up metrics
./benchctl ingest metrics
./benchctl query metrics 20     # 20 runs/query after warmup
./benchctl collect
./benchctl report
```

**Faster feedback — only some engines.** `--only <csv>` (or `BENCH_ONLY` in
`.env`) restricts a run to a subset of systems: it brings up just those engines
plus the lane's ingest driver (skipping the competitors entirely) and
queries/collects/reports only them.

```bash
./benchctl --only oteldb bench-all            # just the embedded engine
./benchctl --only oteldb,oteldb-ch bench-all  # embedded vs ClickHouse, head-to-head
```

Live dashboards while it runs: **Grafana** http://localhost:3000 (every engine
pre-provisioned as a datasource), **cAdvisor** http://localhost:8085.

---

## What you get

`results/REPORT.md` — per-signal latency matrices (rows = query, columns =
system, cell = p90 ms) plus a storage/memory footprint table. Raw per-sample
latencies land in `results/<signal>/raw/` and per-system CSVs in
`results/<signal>/`.

For deeper, query-trace-attributed numbers on PromQL/LogQL, the repo's
`otelbench … bench --trace` (which captures a server-side trace per query and
emits benchstat output) can be pointed at any `addr` from `systems.yml` — see
`../oteldb/dev/local/ch-bench-read`.

---

## Fairness knobs & caveats

- **Resource caps.** `oteldb` is capped at `BENCH_CPUS`/`BENCH_MEMLIMIT`
  (`.env`). For a fair fight, apply equivalent caps to the other services
  (compose `deploy.resources` / each engine's own limits) — left explicit so you
  choose the comparison point.
- **Storage substrate.** oteldb's embedded engine here uses its `file` backend
  on a volume; the same engine speaks S3 to `fs` in production
  (`storage/backend/s3`). The shared-bucket comparison is exact for
  Mimir/Loki/Tempo/GreptimeDB; oteldb's S3 path is wired the same way once
  exposed in its config (`config/oteldb/oteldb.yml`).
- **Native dialects** (VictoriaLogs/VictoriaTraces) are best-effort translations
  in `queries/native/`; they measure the same retrieval work, not identical
  semantics. Queries with no equivalent report `n/a`.
- **Datasets.** Metrics are live `node_exporter` series multiplied to 10 hosts by
  vmagent during a prewarm (`PREWARM` env, default 30s); some engines have PromQL
  dialect gaps (e.g. GreptimeDB rejects `topk` and bare `{__name__=~...}`),
  reported as `ERR`/empty. Logs match
  the loghub suite when `LOGHUB_DIR` is set, else synthetic. Traces are
  `telemetrygen` spans — swap in a richer generator and adjust
  `queries/traces.traceql.yml` for application-shaped traces.
- **Images** are pinned in the compose files / `.env`; bump them deliberately so
  runs stay comparable.

## Layout

```
systems.yml              the matrix (systems × signals × api × lang × storage)
compose/                 base + shared (oteldb, gigapipe) + per-lane stacks
config/                  one dir per engine; otelcol/ holds the fan-out configs
queries/                 PromQL/LogQL/TraceQL suites (+ native/ overrides)
cmd/benchctl/            CLI entrypoint (up · ingest · query · collect · report)
internal/bench/          orchestration: compose, ingest, query, collect, report
results/                 CSVs, raw latencies, REPORT.md
```
