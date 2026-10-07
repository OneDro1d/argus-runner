package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/preflight"
)

// — the compose-tier probes behind preflight.Probes. Read-only, like every other probe
// here: `docker info`, `docker compose config`, `docker ps`, and a TCP connect to 127.0.0.1.

// obsSharedComposeFile is the shared argus-obs project's file, relative to the kit directory
// (`argus init` creates it; preflight, like `argus up`, runs from there).
var obsSharedComposeFile = filepath.Join("deploy", "compose", "docker-compose.obs-shared.yml")

// obsSharedProject is the compose project that file is started as; its own containers holding a port is
// a re-onboard, not a conflict.
const obsSharedProject = "argus-obs"

func runBin(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), strings.TrimSpace(out.String()))
		}
		return "", fmt.Errorf("could not execute %s: %w", name, err)
	}
	return strings.TrimSpace(out.String()), nil
}

// DockerReady asks the daemon, not the CLI: `docker version` succeeds with a client alone.
func (p execProbes) DockerReady() error {
	_, err := runBin("docker", "info", "--format", "{{.ServerVersion}}")
	return err
}

// ComposeHostPorts reads the host ports the shared obs project publishes from its OWN compose file, as
// compose resolves them. Nothing is written down here.
func (p execProbes) ComposeHostPorts() ([]int, error) {
	if _, err := os.Stat(obsSharedComposeFile); err != nil {
		return nil, fmt.Errorf("%v — run `argus preflight` from the kit directory `argus init` created", err)
	}
	out, err := runBin("docker", "compose", "-f", obsSharedComposeFile, "-p", obsSharedProject, "config", "--format", "json")
	if err != nil {
		return nil, err
	}
	return composePortsFromJSON([]byte(out))
}

// composePortsFromJSON extracts the published HOST ports from `docker compose config --format json`. A range
// or an unresolved variable is not a number and is skipped, not guessed at.
func composePortsFromJSON(data []byte) ([]int, error) {
	var cfg struct {
		Services map[string]struct {
			Ports []struct {
				Published interface{} `json:"published"`
			} `json:"ports"`
		} `json:"services"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("could not parse `docker compose config` output: %w", err)
	}
	seen := map[int]bool{}
	var ports []int
	for _, svc := range cfg.Services {
		for _, pt := range svc.Ports {
			var n int
			switch v := pt.Published.(type) {
			case string:
				n, _ = strconv.Atoi(v)
			case float64:
				n = int(v)
			}
			if n > 0 && !seen[n] {
				seen[n] = true
				ports = append(ports, n)
			}
		}
	}
	sort.Ints(ports)
	return ports, nil
}

// HostPortHolder: a running container publishing the port first (it names itself and its project), else a
// TCP connect to 127.0.0.1 for a process outside Docker.
func (p execProbes) HostPortHolder(port int) (preflight.PortHolder, error) {
	out, err := runBin("docker", "ps", "--filter", fmt.Sprintf("publish=%d", port),
		"--format", `{{.Names}}|{{.Label "com.docker.compose.project"}}`)
	if err != nil {
		return preflight.PortHolder{}, err
	}
	if first := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]; first != "" {
		name, proj, _ := strings.Cut(first, "|")
		return preflight.PortHolder{Held: true, Container: name, Project: proj}, nil
	}
	conn, derr := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 500*time.Millisecond)
	if derr == nil {
		conn.Close()
		return preflight.PortHolder{Held: true}, nil
	}
	return preflight.PortHolder{}, nil
}
