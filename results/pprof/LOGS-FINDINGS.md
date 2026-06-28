# oteldb LogQL query performance — investigation (EXPLAIN ANALYZE + pprof)

Image `ghcr.io/oteldb/oteldb:logql-fix` (embedded `file` backend), ~75k log
records in a 300s window. Profiles: `logs-cpu.pb.gz`, `logs-heap.pb.gz` (+ binary
`oteldb.bin`). Driven by the LogQL suite (`queries/logs.logql.yml`).

## The one-line finding

**Storage is not the bottleneck — the LogQL engine is.** EXPLAIN ANALYZE
(`X-Oteldb-Profile: 1`) shows the fetch returns ~75k rows in **3–8 ms**, but the
query spends **80–253 ms of self-time above the fetch**, materializing and
processing every one of those rows.

```
lines_by_level  query 257.8ms (self 253.2ms)  └─ recordengine.fetch 4.5ms rows=74700 series_matched=10
select_service  query  92.5ms (self  84.8ms)  └─ recordengine.fetch 7.7ms rows=74700
status_regex    query  84.0ms (self  80.7ms)  └─ recordengine.fetch 3.4ms rows=74700
json_status     query  86.1ms (self  82.4ms)  └─ recordengine.fetch 3.7ms rows=74700
```

Every query fetches and materializes **all 74700 rows** — even `select_service`
with `limit=1000` (the limit is applied *after* materializing everything).

## Where the engine self-time goes (CPU, 20s under load)

- **Per-row materialization** — `storagebackend.logColumns.recordInto` 15% cum,
  `signal.AppendAttributes` 4% (decoding each row's attributes back into a
  `pcommon.Map`).
- **GC** ~40% cum (`tryDeferToSpanScan` 15%, `scanObjectsSmall`/`scanObject` ~20%,
  `mallocgcSmallScanNoHeader` 10%) — driven by the per-row allocations below.
- **Map-based label sets** ~12% — `aeshashbody` 5% + `mapaccess`/`getWithoutKey`
  ~5% + `maps.Clone` 2% (the label set is a `map[Label]Value`, rebuilt per row).
- **Metric grouping** — `aggregatedLabels.forEach` 8% (the `sum by (level)` path).
- **Sort via reflection** ~6% — `sort.SliceStable` → `reflectlite.Swapper` +
  `typedmemmove` sorting 74700 entries with a reflection-based swapper.

## What allocates (heap alloc_space)

`recordInto` 22% · `LabelSet.Set`+`buildSet`+`NewLabelSet`+`AggregatedLabelsFromSet`+
`aggregatedLabels.By` ≈ **35%** (label sets) · `newRecordCols` 7% · `slices.Grow`
(attr KeyValue) 7% · `pcommon.Map.PutEmpty`+`NewAnyValue*` ≈ 8% (pdata round-trip) ·
`rangeAggIterator.Next`+`fillWindow` ≈ **29%** (the metric-window aggregation that
makes `lines_by_level`/`rate_by_level` the slowest at ~240 ms).

## Fixes, by leverage

1. **Limit pushdown / lazy materialization.** `EvalPipeline` fetches every record
   and `materialize` builds a full label set per row, then truncates to
   `params.Limit` (logs.go:96–103). For the filter queries (`select_service`,
   `status_regex`, `json_status`, ~85 ms) only `limit` entries are kept — push the
   limit into the fetch/materialize so it stops early instead of building 74700
   and discarding 73700. Biggest win for non-metric queries.
2. **Slice-backed / pooled `LabelSet`** instead of `map[Label]Value`. The map per
   row (hash + clone) is ~12% CPU and ~35% of allocs across every query. Reuse the
   set (it's already scratch in `materializeRange`) and avoid `maps.Clone` in the
   aggregated-labels path.
3. **`slices.SortStableFunc` instead of `sort.SliceStable`** in `sortEntries`
   (logs.go:294) — removes the reflection swapper (~6% CPU), a trivial change.
4. **Reuse window buffers in `logqlmetric.rangeAggIterator`** (`Next`/`fillWindow`,
   ~29% of allocs) — the dominant cost of `lines_by_level`/`rate_by_level`; avoid
   re-allocating per step/window and re-scanning overlapping windows.
5. **Skip the `pcommon.Map` round-trip in `recordInto`/`SetFromRecord`** — decode
   stored attributes into a reused buffer rather than rebuilding pdata Values
   (`PutEmpty`/`NewAnyValueStringValue`, ~8% allocs + `AppendAttributes`).

GreptimeDB answers these same queries in 4–7 ms (columnar SQL with indexed
columns + bounded scans); oteldb's gap is almost entirely (1) materializing more
rows than needed and (2) per-row map/pdata allocation, not the fetch.

## Reproduce

```
# per-query plan (server logs the tree):
curl -s -H 'X-Oteldb-Profile: 1' -G localhost:3100/loki/api/v1/query_range \
  --data-urlencode 'query=sum by (level) (count_over_time({service_name="unknown_service"}[1m]))' \
  --data start=<ns> --data end=<ns> --data step=30
docker logs oteldb-bench-oteldb-1 | grep 'EXPLAIN ANALYZE'

go tool pprof -http=: results/pprof/oteldb.bin results/pprof/logs-cpu.pb.gz
```
