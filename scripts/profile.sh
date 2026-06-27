#!/usr/bin/env bash
# Capture oteldb pprof profiles (CPU, in-use heap, allocation delta) under each
# signal's query load. Output: results/pprof/<signal>.{cpu,heap,allocs}.pb.gz +
# .top.txt summaries. Allocs are baseline-diffed so they're attributable to the
# load (not cumulative since process start).
set -u
PPROF=http://127.0.0.1:9010/debug/pprof
OUT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/results/pprof"
mkdir -p "$OUT"
SECS=20

# load_<signal> N : run that signal's representative oteldb queries for N seconds.
load_metrics() {
  local end=$(( $(date +%s) + $1 ))
  while [ "$(date +%s)" -lt "$end" ]; do
    local n=$(date +%s) s=$(( $(date +%s) - 120 ))
    curl -s -o /dev/null -G 'http://127.0.0.1:9090/api/v1/query_range' --data-urlencode 'query=sum by(instance) (rate(node_network_receive_bytes_total{job="node_exporter"}[5m]))' --data "start=$s" --data "end=$n" --data step=15
    curl -s -o /dev/null -G 'http://127.0.0.1:9090/api/v1/query' --data-urlencode 'query=topk(3, avg_over_time(node_memory_MemFree_bytes{job="node_exporter"}[30m]))' --data "time=$n"
    curl -s -o /dev/null -G 'http://127.0.0.1:9090/api/v1/query_range' --data-urlencode 'query=sum by(instance) (irate(node_cpu_seconds_total{job="node_exporter",mode="user"}[1m]))' --data "start=$s" --data "end=$n" --data step=15
  done
}
load_logs() {
  local end=$(( $(date +%s) + $1 ))
  while [ "$(date +%s)" -lt "$end" ]; do
    local n="$(date +%s)000000000" s="$(( $(date +%s) - 300 ))000000000"
    curl -s -o /dev/null -G 'http://127.0.0.1:3100/loki/api/v1/query_range' --data-urlencode 'query={service_name="unknown_service"}' --data "start=$s" --data "end=$n" --data step=30 --data limit=1000
    curl -s -o /dev/null -G 'http://127.0.0.1:3100/loki/api/v1/query_range' --data-urlencode 'query=sum by (level) (count_over_time({service_name="unknown_service"}[1m]))' --data "start=$s" --data "end=$n" --data step=30
    curl -s -o /dev/null -G 'http://127.0.0.1:3100/loki/api/v1/query_range' --data-urlencode 'query={service_name="unknown_service"} | json | status>=400' --data "start=$s" --data "end=$n" --data step=30 --data limit=1000
  done
}
load_traces() {
  local end=$(( $(date +%s) + $1 ))
  while [ "$(date +%s)" -lt "$end" ]; do
    local n=$(date +%s) s=$(( $(date +%s) - 3600 ))
    curl -s -o /dev/null -G 'http://127.0.0.1:3200/api/search' --data-urlencode 'q={ resource.service.name = "frontend" }' --data "start=$s" --data "end=$n" --data limit=20
    curl -s -o /dev/null -G 'http://127.0.0.1:3200/api/search' --data-urlencode 'q={ span.net.peer.ip != "" }' --data "start=$s" --data "end=$n" --data limit=20
    curl -s -o /dev/null -G 'http://127.0.0.1:3200/api/search' --data-urlencode 'q={ duration > 1ms }' --data "start=$s" --data "end=$n" --data limit=20
  done
}

top() { go tool pprof -top -nodecount=30 "$@" 2>/dev/null; }

capture() {
  local sig="$1"
  echo ">> [$sig] baseline allocs"
  curl -s "$PPROF/allocs" -o "$OUT/$sig.allocs.base.pb.gz"
  echo ">> [$sig] starting load + CPU profile (${SECS}s)"
  "load_$sig" $((SECS + 12)) &
  local lpid=$!
  sleep 3
  curl -s "$PPROF/profile?seconds=$SECS" -o "$OUT/$sig.cpu.pb.gz"
  curl -s "$PPROF/heap" -o "$OUT/$sig.heap.pb.gz"          # in-use snapshot while hot
  wait "$lpid" 2>/dev/null
  curl -s "$PPROF/allocs" -o "$OUT/$sig.allocs.pb.gz"      # cumulative after load
  # summaries
  top "$OUT/$sig.cpu.pb.gz"                                              > "$OUT/$sig.cpu.top.txt"
  top -sample_index=inuse_space "$OUT/$sig.heap.pb.gz"                   > "$OUT/$sig.heap.top.txt"
  top -sample_index=alloc_space -base "$OUT/$sig.allocs.base.pb.gz" "$OUT/$sig.allocs.pb.gz" > "$OUT/$sig.allocs.top.txt"
  rm -f "$OUT/$sig.allocs.base.pb.gz"
  echo ">> [$sig] done: $(ls "$OUT"/$sig.*.pb.gz | wc -l) profiles"
}

SIG="${1:?usage: profile.sh <metrics|logs|traces>}"
capture "$SIG"
