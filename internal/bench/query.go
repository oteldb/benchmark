package bench

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// Query replays the canonical suite for a signal against every engine declared
// for it, COUNT times per query after WARMUP, and writes one CSV per engine to
// results/<signal>/. Engines whose lang is not the reference language load an
// id-matched override from queries/native/.
func (e *Env) Query(signal string, count, warmup int) error {
	suite, err := LoadSuite(e.Dir, signal)
	if err != nil {
		return err
	}
	systems, err := LoadSystems(filepath.Join(e.Dir, "systems.yml"))
	if err != nil {
		return err
	}
	out := filepath.Join(e.Dir, "results", signal)
	if err := os.MkdirAll(filepath.Join(out, "raw"), 0o755); err != nil {
		return err
	}
	client := &http.Client{Timeout: 120 * time.Second}

	for _, sys := range systems.For(signal) {
		native, err := LoadNative(e.Dir, sys.Name)
		if err != nil {
			return err
		}
		fmt.Printf(">> [%s] %s (%s) -> %s\n", signal, sys.Name, sys.Lang, sys.Addr)

		rows := [][]string{{"query_id", "lang", "p50_ms", "p90_ms", "p99_ms", "samples", "errors"}}
		for _, q := range suite.Queries {
			useLang, text := sys.Lang, q.Q
			if native != nil {
				useLang = native.API
				t, ok := native.query(q.ID)
				if !ok { // no native equivalent for this query
					rows = append(rows, []string{q.ID, sys.Lang, "n/a", "n/a", "n/a", "0", "0"})
					fmt.Printf("   %-28s n/a\n", q.ID)
					continue
				}
				text = t
			}

			var lat []float64
			errors := 0
			for i := 0; i < warmup; i++ {
				_, _ = e.timeRequest(client, sys.Addr, useLang, q.Type, text, suite)
			}
			for i := 0; i < count; i++ {
				ms, ok := e.timeRequest(client, sys.Addr, useLang, q.Type, text, suite)
				if ok {
					lat = append(lat, ms)
				} else {
					errors++
				}
			}
			p50, p90, p99 := pctl(lat, 50), pctl(lat, 90), pctl(lat, 99)
			rows = append(rows, []string{q.ID, useLang,
				fmtMaybe(p50, len(lat)), fmtMaybe(p90, len(lat)), fmtMaybe(p99, len(lat)),
				strconv.Itoa(len(lat)), strconv.Itoa(errors)})
			writeRaw(filepath.Join(out, "raw", sys.Name+"_"+q.ID+".txt"), lat)
			fmt.Printf("   %-28s p50=%-8s p90=%-8s err=%d\n", q.ID, fmtMaybe(p50, len(lat)), fmtMaybe(p90, len(lat)), errors)
		}
		if err := writeCSV(filepath.Join(out, sys.Name+".csv"), rows); err != nil {
			return err
		}
	}
	fmt.Printf(">> wrote %s/*.csv\n", out)
	return nil
}

// timeRequest issues one query and returns its wall-clock latency in ms, reading
// and discarding the body. ok=false on transport error or non-2xx.
func (e *Env) timeRequest(c *http.Client, addr, lang, typ, q string, s *Suite) (float64, bool) {
	now := time.Now().Unix()
	start := now - int64(s.Range.Lookback)
	u := buildURL(addr, lang, typ, q, start, now, s.Range.Step)
	if u == "" {
		return 0, false
	}

	t0 := time.Now()
	resp, err := c.Get(u)
	if err != nil {
		return 0, false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	ms := float64(time.Since(t0).Microseconds()) / 1000.0
	return ms, resp.StatusCode >= 200 && resp.StatusCode < 300
}

// buildURL assembles the query URL for a (lang,type), or "" if unsupported.
func buildURL(addr, lang, typ, q string, start, end int64, step int) string {
	v := url.Values{}
	switch lang + ":" + typ {
	case "promql:instant":
		v.Set("query", q)
		v.Set("time", strconv.FormatInt(end, 10))
		return addr + "/api/v1/query?" + v.Encode()
	case "promql:range":
		v.Set("query", q)
		v.Set("start", strconv.FormatInt(start, 10))
		v.Set("end", strconv.FormatInt(end, 10))
		v.Set("step", strconv.Itoa(step))
		return addr + "/api/v1/query_range?" + v.Encode()
	case "logql:instant":
		v.Set("query", q)
		v.Set("time", strconv.FormatInt(end*1e9, 10))
		v.Set("limit", "1000")
		return addr + "/loki/api/v1/query?" + v.Encode()
	case "logql:range":
		v.Set("query", q)
		v.Set("start", strconv.FormatInt(start*1e9, 10))
		v.Set("end", strconv.FormatInt(end*1e9, 10))
		v.Set("step", strconv.Itoa(step))
		v.Set("limit", "1000")
		v.Set("direction", "backward")
		return addr + "/loki/api/v1/query_range?" + v.Encode()
	case "traceql:search":
		v.Set("q", q)
		v.Set("start", strconv.FormatInt(start, 10))
		v.Set("end", strconv.FormatInt(end, 10))
		v.Set("limit", "20")
		return addr + "/api/search?" + v.Encode()
	}
	// native dialects key on lang only (type is ignored)
	switch lang {
	case "logsql":
		v.Set("query", q)
		v.Set("start", strconv.FormatInt(start*1000, 10))
		v.Set("end", strconv.FormatInt(end*1000, 10))
		v.Set("limit", "1000")
		return addr + "/select/logsql/query?" + v.Encode()
	case "jaeger":
		// q is a pre-built, already-encoded Jaeger query string from the native file.
		return fmt.Sprintf("%s/select/jaeger/api/traces?%s&start=%d&end=%d&limit=20",
			addr, q, start*1e6, end*1e6)
	}
	return ""
}

func pctl(xs []float64, p int) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	idx := int(float64(p)/100*float64(len(s)) + 0.5)
	if idx < 1 {
		idx = 1
	}
	if idx > len(s) {
		idx = len(s)
	}
	return s[idx-1]
}

func fmtMaybe(v float64, n int) string {
	if n == 0 {
		return "ERR"
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

func writeRaw(path string, xs []float64) {
	if len(xs) == 0 {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	for _, x := range xs {
		fmt.Fprintf(f, "%.1f\n", x)
	}
}

func writeCSV(path string, rows [][]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.WriteAll(rows); err != nil {
		return err
	}
	w.Flush()
	return w.Error()
}
