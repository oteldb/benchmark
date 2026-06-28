package bench

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// composeFiles is the full overlay: base + cross-signal + every lane. Compose
// merges by service name, and profiles gate which lane services actually start.
// The explicit --env-file is required: with -f pointing into compose/, Compose
// sets the project directory there and would otherwise ignore the root .env.
func (e *Env) composeFiles() []string {
	c := filepath.Join(e.Dir, "compose")
	var args []string
	if env := filepath.Join(e.Dir, ".env"); fileExists(env) {
		args = append(args, "--env-file", env)
	}
	for _, f := range []string{"base.yml", "shared.yml", "metrics.yml", "logs.yml", "traces.yml"} {
		args = append(args, "-f", filepath.Join(c, f))
	}
	return args
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func profilesFor(lane string) []string {
	switch lane {
	case "all":
		return []string{"--profile", "metrics", "--profile", "logs", "--profile", "traces"}
	case "metrics", "logs", "traces":
		return []string{"--profile", lane}
	default:
		return nil
	}
}

// compose runs `docker compose <files> <profiles> <args...>` wired to stdio.
func (e *Env) compose(lane string, args ...string) error {
	full := append(e.composeFiles(), profilesFor(lane)...)
	full = append(full, args...)
	cmd := exec.Command("docker", append([]string{"compose"}, full...)...)
	cmd.Dir = e.Dir
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	return cmd.Run()
}

// laneDrivers are the ingest-side services every engine in a lane shares (the
// fan-out collector / scraper). They feed the same data to all backends, so the
// Only mode keeps just these plus the selected engines.
var laneDrivers = map[string][]string{
	"metrics": {"node-exporter", "vmagent"},
	"logs":    {"otelcol-logs"},
	"traces":  {"otelcol-traces"},
}

// engineDeps are non-engine services a given engine needs beyond the lane
// driver. oteldb's file backend is self-contained, so it has none.
var engineDeps = map[string][]string{
	"oteldb-ch":  {"clickhouse-oteldb"},
	"gigapipe":   {"clickhouse"},
	"mimir":      {"fs", "fs-init"},
	"greptimedb": {"fs", "fs-init"},
	"loki":       {"fs", "fs-init"},
	"tempo":      {"fs", "fs-init"},
}

// Up builds and starts a lane (or "all"). With Env.Only set it brings up just
// the selected engines plus the lane's ingest driver — a much faster loop than
// the full matrix (the driver still fans data out to whatever is up).
func (e *Env) Up(lane string) error {
	if profilesFor(lane) == nil {
		return fmt.Errorf("usage: up <metrics|logs|traces|all>")
	}
	if len(e.Only) > 0 {
		return e.upOnly(lane)
	}
	fmt.Printf(">> building + starting lane=%s\n", lane)
	if err := e.compose(lane, "up", "-d", "--build", "--remove-orphans"); err != nil {
		return err
	}
	fmt.Println(">> Grafana: http://localhost:3000   cAdvisor: http://localhost:8085")
	return e.compose(lane, "ps")
}

// upOnly starts only the selected engines and the lane's ingest driver, naming
// services explicitly so profile-gated services start without pulling in the
// other engines.
func (e *Env) upOnly(lane string) error {
	lanes := []string{"metrics", "logs", "traces"}
	if lane != "all" {
		lanes = []string{lane}
	}
	set := map[string]bool{}
	var services []string
	add := func(name string) {
		if !set[name] {
			set[name] = true
			services = append(services, name)
		}
	}
	for name := range e.Only {
		add(name)
		for _, dep := range engineDeps[name] {
			add(dep)
		}
	}
	for _, l := range lanes {
		for _, d := range laneDrivers[l] {
			add(d)
		}
	}

	fmt.Printf(">> building + starting only=%v lane=%s services=%v\n", keys(e.Only), lane, services)
	// No profiles: explicitly named services start regardless of profile gating.
	args := append(e.composeFiles(), "up", "-d", "--build", "--remove-orphans")
	args = append(args, services...)
	cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
	cmd.Dir = e.Dir
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		return err
	}
	return e.compose(lane, "ps")
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Down stops everything and wipes volumes.
func (e *Env) Down() error {
	return e.compose("all", "down", "-v", "--remove-orphans")
}
