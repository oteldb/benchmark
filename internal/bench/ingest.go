package bench

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"
)

// Ingest pushes the canonical dataset once into the lane collector, which fans
// it out byte-for-byte to every backend.
func (e *Env) Ingest(signal string) error {
	switch signal {
	case "metrics":
		return e.ingestMetrics()
	case "logs":
		return e.ingestLogs()
	case "traces":
		return e.ingestTraces()
	default:
		return fmt.Errorf("unknown signal %q", signal)
	}
}

// otelbench runs the repo's otelbench from the sibling oteldb checkout.
func (e *Env) otelbench(args ...string) error {
	full := append([]string{"run", "github.com/oteldb/oteldb/cmd/otelbench"}, args...)
	cmd := exec.Command("go", full...)
	cmd.Dir = e.OteldbSrc
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// ingestMetrics is a prewarm wait: vmagent already scrapes the multiplied
// node_exporter targets and remote-writes the live stream to every engine from
// the moment the lane is up. Here we just let data accumulate (current
// timestamps, in order) so the query window has samples. Duration: PREWARM env
// (default 90s).
func (e *Env) ingestMetrics() error {
	prewarm := 90 * time.Second
	if v := os.Getenv("PREWARM"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			prewarm = d
		}
	}
	fmt.Printf(">> prewarm: vmagent scraping node_exporter → all engines for %s\n", prewarm)
	deadline := time.Now().Add(prewarm)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Second)
		fmt.Printf("   ... %.0fs remaining\n", time.Until(deadline).Seconds())
	}
	fmt.Println(">> prewarm done")
	return nil
}

func (e *Env) ingestLogs() error {
	fmt.Println(">> ingesting logs into otelcol-logs over OTLP")
	args := []string{"otel", "logs", "bench", "--duration", "30s"}
	if e.LoghubDir != "" {
		args = append(args[:3], append([]string{"--source", e.LoghubDir}, args[3:]...)...)
	} else {
		fmt.Println("   (LOGHUB_DIR unset -> synthetic logs; set it for the loghub suite to match)")
	}
	return e.otelbench(append(args, "localhost:4317")...)
}

func (e *Env) ingestTraces() error {
	fmt.Println(">> generating spans into otelcol-traces over OTLP (telemetrygen)")
	cmd := exec.Command("docker", "run", "--rm", "--network", "bench",
		"ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:v0.115.0",
		"traces", "--otlp-insecure", "--otlp-endpoint", "otelcol-traces:4317",
		"--service", "frontend", "--traces", "20000", "--rate", "2000", "--child-spans", "3")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func download(url, dst string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
