package bench

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Correctness statuses for a single (engine, query) probe.
const (
	statusOK    = "OK"    // 2xx and a non-empty result
	statusEmpty = "EMPTY" // 2xx but zero results
	statusErr   = "ERR"   // transport error, non-2xx, or unparseable body
	statusNA    = "N/A"   // native-dialect engine has no equivalent for this query
)

// probeResult is one engine's outcome for one query.
type probeResult struct {
	count  int
	status string
	apples bool
}

// Check validates query *results* (not just latency): it issues each suite query
// once per engine, parses the response into a result count, and compares every
// apples engine against a reference (the first apples engine that returns data,
// i.e. oteldb). It catches the failures a latency run hides — a 200-but-empty
// response from a dialect gap, a broken filter, or missing data. Writes
// results/<signal>/correctness.csv.
func (e *Env) Check(signal string) error {
	suite, err := LoadSuite(e.Dir, signal)
	if err != nil {
		return err
	}
	systems, err := LoadSystems(filepath.Join(e.Dir, "systems.yml"))
	if err != nil {
		return err
	}
	sys := e.selected(systems.For(signal))
	client := &http.Client{Timeout: 120 * time.Second}

	// probes[queryID][engine] = result
	probes := map[string]map[string]probeResult{}
	for _, q := range suite.Queries {
		probes[q.ID] = map[string]probeResult{}
	}

	for _, s := range sys {
		native, err := LoadNative(e.Dir, s.Name)
		if err != nil {
			return err
		}
		fmt.Printf(">> [%s] checking %s (%s)\n", signal, s.Name, s.Lang)
		for _, q := range suite.Queries {
			useLang, text := s.Lang, q.Q
			if native != nil {
				useLang = native.API
				t, ok := native.query(q.ID)
				if !ok {
					probes[q.ID][s.Name] = probeResult{0, statusNA, s.Apples}
					continue
				}
				text = t
			}
			count, status := e.probeQuery(client, s.Addr, useLang, q.Type, text, suite)
			probes[q.ID][s.Name] = probeResult{count, status, s.Apples}
		}
	}

	// Resolve a reference per query (first apples engine that returned data) and
	// score every engine against it.
	rows := [][]string{{"query_id", "engine", "count", "status", "verdict"}}
	pass, fail := 0, 0
	for _, q := range suite.Queries {
		byEngine := probes[q.ID]
		ref, refCount := "", 0
		for _, s := range sys {
			if p := byEngine[s.Name]; s.Apples && p.status == statusOK {
				ref, refCount = s.Name, p.count
				break
			}
		}
		for _, s := range sys {
			p := byEngine[s.Name]
			verdict := verdictFor(s.Name, ref, p.count, refCount, p.status, p.apples)
			switch verdict {
			case "empty", "err":
				fail++
			case "n/a", "none":
			default:
				pass++
			}
			rows = append(rows, []string{q.ID, s.Name, strconv.Itoa(p.count), p.status, verdict})
			fmt.Printf("   %-26s %-12s %-6s %s\n", q.ID, s.Name, glyphFor(verdict), countStr(p.count, p.status))
		}
	}

	out := filepath.Join(e.Dir, "results", signal, "correctness.csv")
	if err := writeCSV(out, rows); err != nil {
		return err
	}
	fmt.Printf(">> wrote %s (%d ok, %d failed)\n", out, pass, fail)
	return nil
}

// verdictFor scores one engine's result against the reference. Non-apples engines
// (native dialects) are judged on non-emptiness only — never on count agreement,
// since they answer a semantically different query over the same data.
func verdictFor(engine, ref string, count, refCount int, status string, apples bool) string {
	switch status {
	case statusNA:
		return "n/a"
	case statusErr:
		return "err"
	case statusEmpty:
		if ref == "" {
			// No apples engine returned data for this query — likely a selector
			// that matches nothing or data outside the window, not an engine fault.
			return "none"
		}
		return "empty"
	}
	// status OK
	switch {
	case engine == ref:
		return "ref"
	case !apples || ref == "":
		return "ok"
	case count == refCount:
		return "match"
	default:
		return "diff"
	}
}

func glyphFor(v string) string {
	switch v {
	case "ref":
		return "★ref"
	case "match", "ok":
		return "✓"
	case "diff":
		return "≠"
	case "empty":
		return "✗∅"
	case "err":
		return "✗ERR"
	case "none":
		return "∅?"
	default:
		return "-"
	}
}

func countStr(n int, status string) string {
	switch status {
	case statusOK:
		return strconv.Itoa(n)
	case statusEmpty:
		return "∅"
	case statusErr:
		return "ERR"
	default:
		return "n/a"
	}
}

// probeQuery issues one query and returns its result count and status. It retries
// once on an empty result to ride out a transient flush gap.
func (e *Env) probeQuery(c *http.Client, addr, lang, typ, q string, s *Suite) (int, string) {
	for attempt := 0; attempt < 2; attempt++ {
		start, end := s.Window(time.Now().Unix())
		u := buildURL(addr, lang, typ, q, start, end, s.Range.Step)
		if u == "" {
			return 0, statusErr
		}
		resp, err := c.Get(u)
		if err != nil {
			return 0, statusErr
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return 0, statusErr
		}
		count, ok := resultCount(lang, body)
		if !ok {
			return 0, statusErr
		}
		if count > 0 {
			return count, statusOK
		}
		if attempt == 0 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	return 0, statusEmpty
}

// resultCount extracts the number of results from a response body for a query
// language. ok=false when the body cannot be parsed as that API's success shape.
func resultCount(lang string, body []byte) (int, bool) {
	switch lang {
	case "promql", "logql":
		// Prometheus/Loki envelope: {"status":"success","data":{"result":[...]}}
		var r struct {
			Status string `json:"status"`
			Data   struct {
				Result []json.RawMessage `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return 0, false
		}
		if r.Status != "" && r.Status != "success" {
			return 0, false
		}
		return len(r.Data.Result), true
	case "traceql":
		var r struct {
			Traces []json.RawMessage `json:"traces"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return 0, false
		}
		return len(r.Traces), true
	case "jaeger":
		var r struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return 0, false
		}
		return len(r.Data), true
	case "logsql":
		// VictoriaLogs streams newline-delimited JSON objects, one per log line.
		n := 0
		for _, line := range strings.Split(string(body), "\n") {
			if strings.TrimSpace(line) != "" {
				n++
			}
		}
		return n, true
	}
	return 0, false
}
