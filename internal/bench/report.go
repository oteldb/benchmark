package bench

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Report merges results/<signal>/<system>.csv plus footprint.csv into
// results/REPORT.md: per-signal latency matrices (rows = query, columns =
// system, cell = p90 ms) and a storage/memory footprint table.
func (e *Env) Report() error {
	systems, err := LoadSystems(filepath.Join(e.Dir, "systems.yml"))
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# oteldb benchmark report\n\n")
	b.WriteString("_Generated from results/. p90 latency in ms; `*` = native dialect (not\n")
	b.WriteString("query-semantics-comparable to the reference language, same dataset/hardware)._\n\n")

	for _, signal := range []string{"metrics", "logs", "traces"} {
		dir := filepath.Join(e.Dir, "results", signal)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		suite, err := LoadSuite(e.Dir, signal)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "## %s — %s\n\n", signal, Signals[signal].Lang)

		// Engines with a results file, in matrix order.
		var cols []System
		p90 := map[string]map[string]string{} // system -> id -> p90
		for _, sys := range e.selected(systems.For(signal)) {
			data, ok := readP90(filepath.Join(dir, sys.Name+".csv"))
			if !ok {
				continue
			}
			cols = append(cols, sys)
			p90[sys.Name] = data
		}
		if len(cols) == 0 {
			b.WriteString("_no results_\n\n")
			continue
		}

		b.WriteString("| query ")
		for _, c := range cols {
			mark := ""
			if !c.Apples {
				mark = "*"
			}
			fmt.Fprintf(&b, "| %s%s ", c.Name, mark)
		}
		b.WriteString("|\n|---")
		for range cols {
			b.WriteString("|---")
		}
		b.WriteString("|\n")

		for _, q := range suite.Queries {
			fmt.Fprintf(&b, "| `%s` ", q.ID)
			for _, c := range cols {
				v := p90[c.Name][q.ID]
				if v == "" {
					v = "–"
				}
				fmt.Fprintf(&b, "| %s ", v)
			}
			b.WriteString("|\n")
		}
		b.WriteString("\n")
	}

	if rows, ok := readCSV(filepath.Join(e.Dir, "results", "footprint.csv")); ok {
		b.WriteString("## Footprint\n\n")
		b.WriteString("| system | storage | disk (MiB) | mem (MiB) |\n|---|---|---|---|\n")
		for _, r := range rows[1:] { // skip header
			disk, _ := strconv.ParseFloat(r[2], 64)
			fmt.Fprintf(&b, "| %s | %s | %.1f | %s |\n", r[0], r[1], disk/1048576, r[3])
		}
		b.WriteString("\n")
	}

	path := filepath.Join(e.Dir, "results", "REPORT.md")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf(">> wrote %s\n\n%s", path, b.String())
	return nil
}

// readP90 returns id -> p90_ms from a results CSV.
func readP90(path string) (map[string]string, bool) {
	rows, ok := readCSV(path)
	if !ok {
		return nil, false
	}
	out := map[string]string{}
	for _, r := range rows[1:] {
		if len(r) >= 4 {
			out[r[0]] = r[3] // p90_ms column
		}
	}
	return out, true
}

func readCSV(path string) ([][]string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) == 0 {
		return nil, false
	}
	return rows, true
}
