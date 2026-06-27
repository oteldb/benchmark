// Command benchctl drives the oteldb cross-engine benchmark: bring the
// docker-compose matrix up, ingest the canonical dataset, replay the query
// suites against every engine, capture the footprint, and render the report.
//
// Usage:
//
//	benchctl up      [metrics|logs|traces|all]
//	benchctl ingest  <metrics|logs|traces>
//	benchctl query   <metrics|logs|traces> [runs]
//	benchctl collect
//	benchctl report
//	benchctl bench   <metrics|logs|traces> [runs]   # up→ingest→settle→query→collect
//	benchctl bench-all [runs]                        # all lanes + report
//	benchctl down
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/oteldb/benchmark/internal/bench"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	e, err := bench.LoadEnv()
	if err != nil {
		return err
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "up":
		return e.Up(arg(rest, 0, "all"))
	case "down":
		return e.Down()
	case "ingest":
		return e.Ingest(must(rest, 0, "ingest <metrics|logs|traces>"))
	case "query":
		return e.Query(must(rest, 0, "query <metrics|logs|traces> [runs]"), runs(rest, 1, e.Runs), 3)
	case "collect":
		return e.Collect()
	case "report":
		return e.Report()
	case "bench":
		return benchLane(e, must(rest, 0, "bench <metrics|logs|traces> [runs]"), runs(rest, 1, e.Runs))
	case "bench-all":
		n := runs(rest, 0, e.Runs)
		for _, lane := range []string{"metrics", "logs", "traces"} {
			if err := benchLane(e, lane, n); err != nil {
				return err
			}
		}
		return e.Report()
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// benchLane runs one lane end-to-end: up → ingest → settle → query → footprint.
func benchLane(e *bench.Env, lane string, n int) error {
	if err := e.Up(lane); err != nil {
		return err
	}
	fmt.Println(">> waiting 10s for services to become healthy")
	time.Sleep(10 * time.Second)
	if err := e.Ingest(lane); err != nil {
		return err
	}
	fmt.Println(">> waiting 30s for flush/compaction")
	time.Sleep(30 * time.Second)
	if err := e.Query(lane, n, 3); err != nil {
		return err
	}
	return e.Collect()
}

func usage() {
	fmt.Print(`benchctl — oteldb cross-engine benchmark

  up      [metrics|logs|traces|all]    build + start a lane (default all)
  ingest  <metrics|logs|traces>        push canonical dataset (fan-out)
  query   <metrics|logs|traces> [runs] replay suite against every engine
  collect                              capture storage + memory footprint
  report                               render results/REPORT.md
  bench   <metrics|logs|traces> [runs] up→ingest→settle→query→collect
  bench-all [runs]                     all lanes, then report
  down                                 stop + wipe volumes
`)
}

func arg(a []string, i int, def string) string {
	if i < len(a) {
		return a[i]
	}
	return def
}

func must(a []string, i int, help string) string {
	if i >= len(a) {
		fmt.Fprintf(os.Stderr, "usage: benchctl %s\n", help)
		os.Exit(2)
	}
	return a[i]
}

func runs(a []string, i, def int) int {
	if i < len(a) {
		if n, err := strconv.Atoi(a[i]); err == nil {
			return n
		}
	}
	return def
}
