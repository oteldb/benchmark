# LogQL query performance — storage-level pushdown proposals

Audience: someone working in `/src/oteldb/storage` (+ the embedded
`internal/storagebackend` adapter in `/src/oteldb/oteldb`). Companion to
`results/pprof/LOGS-FINDINGS.md` (the raw EXPLAIN ANALYZE + pprof evidence).

## TL;DR

The LogQL slowness is **engine-side materialization, not the fetch**. EXPLAIN
ANALYZE: the storage fetch returns ~75k rows in **3–8 ms**, but the query spends
**80–253 ms above it** building a label set per row and (for metric queries)
bucketing all samples in Go. Two storage-level pushdowns remove most of that, and
**both extension points already exist in the codebase** — one is implemented for
ClickHouse but not for the embedded engine.

| Bottleneck (measured) | Pushdown | Status in tree |
|---|---|---|
| Metric queries `lines_by_level`/`rate_by_level` ~240 ms | range-agg step bucketing (`EvalBucketedSample`) | **interface exists; chstorage implements it, embedded does NOT** |
| Filter queries `select_service`/`status_regex` ~85 ms | `Limit` + ordered top-N in the fetch | `fetch.Request` has no `Limit`/order field yet |

---

## Proposal A — push range-aggregation bucketing into the record engine (biggest win)

**Problem.** `sum by (level) (count_over_time({…}[1m]))` currently streams every
matching record to the engine, which builds a `LabelSet` per row and buckets
samples in Go. Profile: `logqlmetric.rangeAggIterator.Next`/`fillWindow` ≈ **29%
of allocations**; storage fetch is 4.5 ms of the 257 ms.

**The hook already exists.** `internal/logql/logqlengine/engine_plan.go`:

```go
// BucketedSampleNode is an optional capability of [SampleNode] implementations
// that can push range-aggregation step-bucketing (e.g.
// sum by(...) (count_over_time(...))) down into the storage layer, instead of
// streaming raw samples for [RangeAggregation] to bucket in Go.
type BucketedSampleNode interface {
    EvalBucketedSample(ctx context.Context, params EvalParams, window time.Duration) (StepIterator, error)
}
```

- **chstorage already implements it** → `internal/chstorage/querier_logs_node.go`
  (oteldb-ch pushes the bucketing into ClickHouse; that's why oteldb-ch logs
  metric queries are fast and the embedded engine is ~30× slower).
- **The embedded `logStreamNode` does NOT** (`internal/storagebackend/logs.go`),
  so the engine falls back to the raw `SampleNode` path + `rangeAggIterator`.

**What to build.** Implement `EvalBucketedSample` on the embedded record-engine
path: scan the grouping column(s) (e.g. `level`/`severity`) + timestamps
columnarly, fold into step-aligned trailing windows, and emit one `Step` per
(group, output step) — never materializing a per-row `LabelSet`. Precedent to
mirror is the **metrics vertical**: `storage/engine/aggregate.go`
`AggregateRange` (`aggPushdownSafe`, `bucketSeries`), which already does
step-bucketed aggregation and even short-circuits from the **stats sidecar** when
the plan is pushdown-safe. The record engine wants the analogous
"grouped count/rate over time buckets" fetch mode.

**Scope of support (start small, expand):** `count_over_time` and `rate`
(rate = count/window) grouped by stored label columns are the common, fully
columnar cases (cover the suite's `lines_by_level`/`rate_by_level`/`error_rate`).
`bytes_over_time`, parser-derived group keys (`| json | …`), and unwrap can stay
on the Go fallback initially.

**Expected impact:** `lines_by_level`/`rate_by_level` from ~240 ms toward the
single-digit ms the columnar scan implies (GreptimeDB does the equivalent
`GROUP BY severity_text` in ~7 ms).

---

## Proposal B — `Limit` + ordered top-N in the fetch (filter queries)

**Problem.** `EvalPipeline` (`internal/storagebackend/logs.go:66–106`) fetches
**every** record in the window, `materialize` builds a full `LabelSet` per row,
then it sorts (`sortEntries`, `sort.SliceStable` → reflection swapper) and
truncates to `params.Limit` (lines 102–103). For `select_service` with
`limit=1000` it builds 74 700 label sets and throws away 73 700.

**Today `fetch.Request` cannot express it.** It has `Matchers`, `Conditions`,
`SecondPass`, `Projection`, `Recycle` — filtering and column projection push
down, but there is **no `Limit` and no scan direction**. Records are stored
timestamp-ordered within a part (timestamp is the part sort key,
`recordengine/schema.go`), so ordered top-N is natural.

**What to build.**
1. Add `Limit int` + `Reverse bool` (or a `Direction`) to `fetch.Request`.
2. Record engine: scan parts in timestamp order for the requested direction,
   apply `Matchers`/`Conditions`/`SecondPass` during the scan, and **stop after
   `Limit` surviving rows** (merge across overlapping parts to keep global order).
3. `storagebackend` sets `req.Limit`/direction from `EvalParams.Limit`/`Direction`
   when the LogQL pipeline is limit-safe (selector + already-offloaded line
   filters; no downstream parser/label-format that changes the result set).
   When safe, this also lets the engine **drop the sort** entirely (storage
   returns ordered rows).

**Limit-safety gate.** Only push the limit when every pipeline stage between the
selector and the limit is already offloaded to the fetch (the optimizer knows
this — it sets `n.conditions`/`pipelineLabels`). `{…}` and `{…} |~ "…"` qualify;
`| json | status>=400` does not (the parsed field is consumed after the limit) —
unless that predicate is itself pushed (see Proposal C).

**Expected impact:** `select_service`/`status_regex` from ~85 ms toward the
~5–8 ms fetch floor (materialize 1 000 instead of 74 700; no reflection sort).

---

## Proposal C (optional) — push the json-status predicate as a SecondPass

`{…} | json | status>=400` currently parses + filters in the engine, so neither
filtering nor limit can be pushed. `fetch.Request.SecondPass func(*Batch) bool`
already exists for "predicates not expressible as a single-column Match" (it sees
the candidate row's materialized batch). A `| json | <field> <cmp> <v>` whose
parsed field is used **only** for filtering could be lowered to a `SecondPass`
over the body column. That drops the rows at the fetch layer and then re-enables
Proposal B's limit pushdown for these queries too. Bigger optimizer change;
lower priority than A and B.

---

## What pushdown does NOT solve (engine-side, complementary)

For queries that must still return many rows through a non-pushable pipeline, the
per-row cost remains. These are independent of storage and worth doing alongside:

- **`LabelSet` is a `map[Label]Value`** (`logqlabels/label_set.go`), rebuilt per
  row → ~35% of allocs (`Set`/`buildSet`/`AggregatedLabelsFromSet`/`maps.Clone`,
  `aeshashbody`). A slice-backed / pooled set helps every non-aggregated query.
- **`recordInto` rebuilds a `pcommon.Map` per row** (`PutEmpty`/`NewAnyValue*`,
  `AppendAttributes`, `slices.Grow`) → ~22% allocs. Decode into a reused buffer.
- **`sortEntries` uses `sort.SliceStable`** (reflection swapper, ~6% CPU) →
  `slices.SortStableFunc`. Trivial, and moot once Proposal B returns ordered rows.

Pushdown reduces how often (and over how many rows) these run; the engine fixes
cap the worst case.

---

## Suggested order

1. **Proposal A** (`EvalBucketedSample` on the embedded path) — largest win, the
   interface + a working ClickHouse reference + the metrics `AggregateRange`
   precedent already exist.
2. **Proposal B** (`fetch.Request.Limit` + ordered scan) — smaller, self-contained,
   also kills the engine sort.
3. Engine-side `LabelSet`/`recordInto`/sort cleanups as a complement.
4. Proposal C if `| json | …`-style filter queries matter.

## Reproduce / verify

```
# per-query plan (set X-Oteldb-Profile: 1, server logs the operator tree):
curl -s -H 'X-Oteldb-Profile: 1' -G localhost:3100/loki/api/v1/query_range \
  --data-urlencode 'query=sum by (level) (count_over_time({service_name="unknown_service"}[1m]))' \
  --data start=<ns> --data end=<ns> --data step=30
docker logs oteldb-bench-oteldb-1 | grep 'EXPLAIN ANALYZE'

go tool pprof -http=: results/pprof/oteldb.bin results/pprof/logs-cpu.pb.gz
# bench just these two engines after a change:
./benchctl --only oteldb,oteldb-ch bench logs && ./benchctl --only oteldb,oteldb-ch report
```
Key references: `engine_plan.go` (`BucketedSampleNode`), `internal/chstorage/
querier_logs_node.go` (reference impl), `storage/engine/aggregate.go`
(`AggregateRange` precedent), `internal/storagebackend/logs.go` (`EvalPipeline`,
`materialize`, `sortEntries`), `storage/query/fetch` (`Request`).
