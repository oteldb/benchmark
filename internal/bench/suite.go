package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Signals maps a lane to its reference query language and canonical suite file.
var Signals = map[string]struct {
	Lang  string
	Suite string
}{
	"metrics": {"PromQL", "queries/metrics.promql.yml"},
	"logs":    {"LogQL", "queries/logs.logql.yml"},
	"traces":  {"TraceQL", "queries/traces.traceql.yml"},
}

// Query is one entry in a canonical suite.
type Query struct {
	ID    string `yaml:"id"`
	Title string `yaml:"title"`
	Type  string `yaml:"type"` // range|instant|search
	Q     string `yaml:"q"`
}

// Suite is a parsed canonical query suite (queries/<signal>.*.yml).
type Suite struct {
	Kind  string `yaml:"kind"`
	Range struct {
		// Lookback is the window size in seconds back from "now" — used for
		// live-generated datasets (logs/traces).
		Lookback int `yaml:"lookback"`
		// Start/End (RFC3339) pin an absolute window — used for static datasets
		// like req.rwq whose timestamps are fixed. When set, they override Lookback.
		Start string `yaml:"start"`
		End   string `yaml:"end"`
		Step  int    `yaml:"step"`
	} `yaml:"range"`
	Queries []Query `yaml:"queries"`
}

// Window returns the [start, end] epoch seconds for this suite: the absolute
// Start/End if pinned, otherwise [now-Lookback, now].
func (s *Suite) Window(now int64) (int64, int64) {
	if s.Range.Start != "" && s.Range.End != "" {
		st, err1 := time.Parse(time.RFC3339, s.Range.Start)
		en, err2 := time.Parse(time.RFC3339, s.Range.End)
		if err1 == nil && err2 == nil {
			return st.Unix(), en.Unix()
		}
	}
	return now - int64(s.Range.Lookback), now
}

// LoadSuite reads the canonical suite for a signal.
func LoadSuite(dir, signal string) (*Suite, error) {
	meta, ok := Signals[signal]
	if !ok {
		return nil, fmt.Errorf("unknown signal %q", signal)
	}
	data, err := os.ReadFile(filepath.Join(dir, meta.Suite))
	if err != nil {
		return nil, err
	}
	var s Suite
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse suite %s: %w", meta.Suite, err)
	}
	if s.Range.Step == 0 {
		s.Range.Step = 60
	}
	return &s, nil
}

// Native is an id-matched override for an engine that does not speak the
// reference language (queries/native/<system>.yml).
type Native struct {
	System  string `yaml:"system"`
	API     string `yaml:"api"` // logsql|jaeger
	Queries []struct {
		ID     string `yaml:"id"`
		Q      string `yaml:"q"`
		Params string `yaml:"params"`
	} `yaml:"queries"`
}

// LoadNative returns the native override for a system, or (nil, nil) if none.
func LoadNative(dir, system string) (*Native, error) {
	path := filepath.Join(dir, "queries", "native", system+".yml")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var n Native
	if err := yaml.Unmarshal(data, &n); err != nil {
		return nil, fmt.Errorf("parse native %s: %w", path, err)
	}
	return &n, nil
}

// query returns the override query text for an id, and whether it exists.
func (n *Native) query(id string) (string, bool) {
	for _, q := range n.Queries {
		if q.ID == id {
			if q.Q != "" {
				return q.Q, true
			}
			return q.Params, true
		}
	}
	return "", false
}
