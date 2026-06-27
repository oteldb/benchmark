package bench

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// composeFiles is the full overlay: base + cross-signal + every lane. Compose
// merges by service name, and profiles gate which lane services actually start.
func (e *Env) composeFiles() []string {
	c := filepath.Join(e.Dir, "compose")
	var args []string
	for _, f := range []string{"base.yml", "shared.yml", "metrics.yml", "logs.yml", "traces.yml"} {
		args = append(args, "-f", filepath.Join(c, f))
	}
	return args
}

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

// Up builds and starts a lane (or "all").
func (e *Env) Up(lane string) error {
	if profilesFor(lane) == nil {
		return fmt.Errorf("usage: up <metrics|logs|traces|all>")
	}
	fmt.Printf(">> building + starting lane=%s\n", lane)
	if err := e.compose(lane, "up", "-d", "--build", "--remove-orphans"); err != nil {
		return err
	}
	fmt.Println(">> Grafana: http://localhost:3000   cAdvisor: http://localhost:8085")
	return e.compose(lane, "ps")
}

// Down stops everything and wipes volumes.
func (e *Env) Down() error {
	return e.compose("all", "down", "-v", "--remove-orphans")
}
