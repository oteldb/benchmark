// Package bench is the engine behind benchctl: it parses the matrix
// (systems.yml), the query suites, and orchestrates docker compose, query
// replay, footprint collection and reporting — the Go replacement for the
// former shell scripts.
package bench

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Env holds resolved paths and .env-derived settings shared by every command.
type Env struct {
	Dir       string // benchmark root (holds systems.yml, compose/, config/, ...)
	OteldbSrc string // sibling oteldb checkout used to run otelbench during ingest
	Runs      int    // benchmark runs per query (after warmup)
	LoghubDir string // optional dir of loghub .log files for the LogQL suite
}

// LoadEnv finds the benchmark root (the dir containing systems.yml, walking up
// from the working directory) and overlays values from its .env file and the
// process environment.
func LoadEnv() (*Env, error) {
	dir, err := findRoot()
	if err != nil {
		return nil, err
	}
	e := &Env{Dir: dir, OteldbSrc: filepath.Join(dir, "..", "oteldb"), Runs: 20}
	kv := readDotenv(filepath.Join(dir, ".env"))
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		if v, ok := kv[k]; ok {
			return v
		}
		return def
	}
	if v := get("OTELDB_SRC", ""); v != "" {
		if filepath.IsAbs(v) {
			e.OteldbSrc = v
		} else {
			e.OteldbSrc = filepath.Join(dir, v)
		}
	}
	if v := get("BENCH_RUNS", ""); v != "" {
		fmt.Sscanf(v, "%d", &e.Runs)
	}
	e.LoghubDir = get("LOGHUB_DIR", "")
	return e, nil
}

func findRoot() (string, error) {
	d, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "systems.yml")); err == nil {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("systems.yml not found in any parent of the working directory")
		}
		d = parent
	}
}

func readDotenv(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// System is one engine's declaration for one signal in systems.yml.
type System struct {
	Name    string `yaml:"-"`
	Addr    string `yaml:"addr"`
	Lang    string `yaml:"lang"`
	Apples  bool   `yaml:"apples"`
	Storage string `yaml:"storage"`
}

// Systems is the parsed matrix, preserving file order within each signal so the
// report columns stay stable.
type Systems struct {
	bySignal map[string][]System
}

// LoadSystems parses systems.yml, keeping per-signal insertion order via
// yaml.Node (Go maps would randomize it).
func LoadSystems(path string) (*Systems, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	s := &Systems{bySignal: map[string][]System{}}
	if len(doc.Content) == 0 {
		return s, nil
	}
	root := doc.Content[0] // mapping: signal -> {system -> System}
	for i := 0; i+1 < len(root.Content); i += 2 {
		signal := root.Content[i].Value
		systemsNode := root.Content[i+1]
		for j := 0; j+1 < len(systemsNode.Content); j += 2 {
			name := systemsNode.Content[j].Value
			var sys System
			if err := systemsNode.Content[j+1].Decode(&sys); err != nil {
				return nil, fmt.Errorf("decode %s.%s: %w", signal, name, err)
			}
			sys.Name = name
			s.bySignal[signal] = append(s.bySignal[signal], sys)
		}
	}
	return s, nil
}

// For returns the engines declared for a signal, in file order.
func (s *Systems) For(signal string) []System { return s.bySignal[signal] }
