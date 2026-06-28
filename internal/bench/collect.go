package bench

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const project = "oteldb-bench"

// Collect captures each engine's on-disk footprint (its go-faster/fs bucket if
// S3-backed, plus its named volume) and live RSS, writing results/footprint.csv.
func (e *Env) Collect() error {
	systems, err := LoadSystems(filepath.Join(e.Dir, "systems.yml"))
	if err != nil {
		return err
	}
	path := filepath.Join(e.Dir, "results", "footprint.csv")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	rows := [][]string{{"system", "storage", "disk_bytes", "mem_mb"}}

	seen := map[string]bool{}
	for _, signal := range []string{"metrics", "logs", "traces"} {
		for _, sys := range e.selected(systems.For(signal)) {
			if seen[sys.Name] {
				continue
			}
			seen[sys.Name] = true

			var disk int64
			switch sys.Storage {
			case "fs":
				disk = bucketBytes(sys.Name) + volBytes(sys.Name+"-data")
			case "local":
				disk = volBytes(sys.Name + "-data")
			case "clickhouse":
				vol := sys.Volume
				if vol == "" {
					vol = "clickhouse-data"
				}
				disk = volBytes(vol)
			}
			mem := memMB(sys.Name)
			rows = append(rows, []string{sys.Name, sys.Storage, strconv.FormatInt(disk, 10), strconv.FormatInt(mem, 10)})
			fmt.Printf("   %-16s storage=%-10s disk=%-12d mem_mb=%d\n", sys.Name, sys.Storage, disk, mem)
		}
	}
	if err := writeCSV(path, rows); err != nil {
		return err
	}
	fmt.Printf(">> wrote %s\n", path)
	return nil
}

func dockerOut(args ...string) string {
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// volBytes is the size of a project-prefixed named volume, 0 if absent.
func volBytes(name string) int64 {
	vol := project + "_" + name
	if dockerOut("volume", "inspect", vol) == "" {
		return 0
	}
	out := dockerOut("run", "--rm", "-v", vol+":/v", "alpine:3.20", "du", "-sb", "/v")
	return firstInt(out)
}

// bucketBytes is the size of one bucket inside the shared fs volume.
func bucketBytes(bucket string) int64 {
	out := dockerOut("run", "--rm", "-v", project+"_fs-data:/v", "alpine:3.20", "du", "-sb", "/v/"+bucket)
	return firstInt(out)
}

// memMB is the current RSS (MiB) of a compose service's container.
func memMB(service string) int64 {
	cid := dockerOut("ps", "-q",
		"-f", "label=com.docker.compose.service="+service,
		"-f", "label=com.docker.compose.project="+project)
	if cid == "" {
		return 0
	}
	if i := strings.IndexByte(cid, '\n'); i >= 0 {
		cid = cid[:i]
	}
	usage := dockerOut("stats", "--no-stream", "--format", "{{.MemUsage}}", cid)
	field := strings.TrimSpace(strings.SplitN(usage, "/", 2)[0])
	switch {
	case strings.HasSuffix(field, "GiB"):
		return int64(parseFloat(strings.TrimSuffix(field, "GiB")) * 1024)
	case strings.HasSuffix(field, "MiB"):
		return int64(parseFloat(strings.TrimSuffix(field, "MiB")))
	default:
		return 0
	}
}

func firstInt(s string) int64 {
	for _, f := range strings.Fields(s) {
		if n, err := strconv.ParseInt(f, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}
