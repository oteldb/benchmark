# oteldb query-path profiling — findings

Captured by `scripts/profile.sh` under each signal's query load against oteldb
(`ghcr.io/oteldb/oteldb:0245c0a4`, embedded `file` backend, unconstrained CPU).
Profiles in this dir: `<signal>.{cpu,heap,allocs}.pb.gz`, binary `oteldb.bin`.
CPU = 20 s under load; `heap` = in-use snapshot taken hot; `allocs` =
alloc_space **baseline-diffed** over the load window (attributable, not
cumulative-since-start).

Explore:
```
go tool pprof -http=: results/pprof/oteldb.bin results/pprof/logs.allocs.pb.gz
go tool pprof -list 'logColumns.record' results/pprof/oteldb.bin results/pprof/logs.allocs.pb.gz
```

## The one-paragraph summary

Same two-front split as before. **Resident storage (in-use heap)** is identical
across signals — the live in-memory state: metrics head `sampleBuf.appendSample`
**~33%** and the record engine `recordCols.appendClone`+`cloneBytes` **~37%**
(clone-on-append copies every column value into a fresh slab). Those own ~70% of
RSS; shrink them to cut footprint. **Query latency** is allocation/GC bound and
the hot spot keeps moving release-to-release: on **metrics** the new #1 is
`engine.sortedWindow` (37% of allocs, 22% CPU — it sorts+copies each window);
**logs** de-concentrated from the 97%-`newRecordCols` spike of `3cc3f30e` into a
spread (`logColumns.record` 43% cum, `signal.DecodeAttributes` 19%,
`logqlabels.LabelSet.Set` 17.5%, `newRecordCols` 10%) and now saturates cores
(450% CPU util); **traces** are unchanged — a `SelectSpansets → scanSpans →
materializeSpans` chain at ~45% GC. Highest-leverage fix is still **pool/stop
cloning `recordCols`** plus killing the per-row attribute re-materialization
(`DecodeAttributes` + `pcommon.Map` rebuild) that is now visible on both logs and
traces.

## Per signal

### metrics — now sortedWindow-bound
- **CPU (~42% util):** `engine.sortedWindow` **22% cum** (with
  `slices.partialInsertionSort` 4.7% on the `{ts,val,sf}` tuples), GC moderate
  (`scanObject` 14.6% cum, `tryDefer` 8.9%, `spanClass` 5%, `memclr` 2.85%),
  `memmove` 3.9%.
- **Allocs:** `engine.sortedWindow` **37.4% cum** (1.97 GB flat) — sorts and
  copies each fetch window; `promql.floatSamples` 17%, `ringbuffer.Push` 12%,
  `engine.windowCopy` 11%, `ensureCap[int64|float64]` ~8.5% (decode buffers grown
  not pooled), prometheus `ScratchBuilder.Add` 3.5%.
- **Levers:** sort window indices in place / reuse the `{ts,val,sf}` scratch;
  pool `windowCopy`/`floatSamples` and pre-size `ensureCap` to the known point
  count; reuse ringbuffer backing arrays across steps.

### logs — de-concentrated, now core-saturating
- **CPU (~450% util — heavily parallel):** GC ~40% (`scanObject` 16% cum,
  `tryDefer` 10.6%, `spanClass` 4.5%) but real work now dominates: `memmove`
  7.5%, `signal.DecodeAttributes` 11.4% cum, `aeshashbody` 2.7% (map hashing for
  label sets).
- **Allocs:** `storagebackend.logColumns.record` **43.4% cum** (16.3 GB flat);
  `signal.DecodeAttributes` 19.4%; `logqlabels.LabelSet.Set` 17.5% +
  `NewLabelSet` 3% (label-set maps rebuilt per line); `newRecordCols` 10.2%;
  `pcommon.Map.PutEmpty` 7.8% + `NewAnyValueStringValue` 3% (pdata rebuild);
  `bytealg.MakeNoZero` 6.25%.
- **Levers:** decode attributes into reused buffers (kills ~19%); reuse
  `LabelSet` maps instead of `Set`/`NewLabelSet` per line (~20%); pool
  `recordCols`; avoid the `pcommon.Map` round-trip in `logColumns.record`.
- **Note:** the v`3cc3f30e` pathology (`newRecordCols` 97% of logs allocs) is
  **gone** — work is now spread, so logs runs much wider (33→450% CPU util) but
  the per-line attribute/label-set rebuild is the new dominant cost.

### traces — unchanged SelectSpansets → scanSpans → materializeSpans chain
- **CPU (193% util; ~45% GC):** `scanObject` 33% cum, `tryDefer` 18%, `spanClass`
  7.8%, `memclr` 6.5%; work: `recordCols.appendRow` 4%, `DecodeAttributes`.
- **Allocs:** `SelectSpansets` 98% cum → `scanSpans` 71% cum, `materializeSpans`
  34% cum (8.4 GB flat), `newRecordCols` 12.3%, `permute[[]uint8]` 9.5% +
  `permute[int64]` 2.8% (byte copies during sort), `DecodeAttributes` 8%,
  `pcommon.Map.PutEmpty` 6.3% + `NewMap`/`NewAnyValueStringValue` ~3%
  (`otelAttrs` 12.7% cum rebuilding pdata).
- **Levers:** pool `recordCols`; sort by index-permute not `permute` byte copies;
  decode attributes into reused buffers; avoid rebuilding `pcommon.Map` per span.

## Cross-cutting (do these first — ordered by leverage)
1. **`recordengine.recordCols` — pool on read, stop cloning on resident.** 37% of
   resident heap (`appendClone`/`cloneBytes`) and a top-5 query allocator on every
   signal. Pool the column set on the fetch path; store resident values as offsets
   into one shared byte slab instead of `cloneBytes` per value. (`storage/recordengine`)
2. **`signal.DecodeAttributes` + `pcommon.Map` rebuild — reuse buffers, skip the
   round-trip.** Now ~19% of logs allocs and ~14% of traces (DecodeAttributes +
   PutEmpty/NewMap). Decode into reused buffers; don't rebuild `pcommon.Map` per
   row/span. (`storage/signal`, `internal/storagebackend`)
3. **`engine.sortedWindow` — sort in place / reuse scratch.** New metrics #1 (37%
   allocs, 22% CPU). (`storage/engine`)
4. **`logqlabels.LabelSet` — reuse maps.** ~20% of logs allocs via `Set`/`NewLabelSet`.
   (`internal/logql/logqlengine/logqlabels`)
5. **`sampleBuf.appendSample` — 33% of resident heap.** Metrics head in-memory
   samples; chunk/flush sooner or pack denser to cut RSS. (`storage/engine`)
6. GC tuning (`GOGC`/soft memlimit) is a band-aid; the allocation fixes are the cure.

## What changed vs 3cc3f30e (for trend tracking)
- **Metrics:** `windowCopy` (was 41% of allocs) dropped to 11%; new top is
  `sortedWindow` 37% + `ensureCap` reappearing. CPU util 33→42%.
- **Logs: de-concentrated.** `newRecordCols` 97% → 10%; cost spread across
  `logColumns.record`/`DecodeAttributes`/`LabelSet.Set`. CPU util 58→450% (now
  parallelizes instead of serializing on one allocator) — wall-clock p90s
  basically flat (see REPORT.md), so this is a throughput/GC-pressure shift.
- **Traces:** essentially identical shape and magnitudes.
- **Resident heap:** unchanged shape — `sampleBuf` + `recordCols` clone still
  dominate; storage-footprint levers are the same.

## Methodology caveats
- Metrics captured with **vmagent paused** (clean read, no live-ingest cache
  churn); logs/traces queried over their ingested dataset with collectors idle.
- **In-use heap is a whole-process snapshot** — includes the resident ingestion
  head (`ConsumeMetrics`/`ConsumeLogs`), which is exactly the in-RAM storage to
  shrink, so it's reported as-is.
- Single-node `file` backend; the S3 backend would add object-fetch latency/allocs
  not seen here.
