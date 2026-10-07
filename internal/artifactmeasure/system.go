package artifactmeasure

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/envcapture"
)

// Func measures the running system under test. It never returns an error: every way of not knowing is a
// Reading with a Reason, because "could not measure" is a result the run must record, not a failure.
type Func func(ctx context.Context) Reading

// CmdRunner runs one command and returns its stdout. exec in production, a canned map in tests.
type CmdRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

const measureTimeout = 20 * time.Second

// K8s reads the digests of the containers running in namespace ns through the API.
func K8s(cl *envcapture.Client, ns string) Func {
	return func(ctx context.Context) Reading {
		r := Reading{Source: "k8s"}
		ctx, cancel := context.WithTimeout(ctx, measureTimeout)
		defer cancel()
		cs, reason := envcapture.ReadRunningContainers(ctx, cl, ns)
		if reason != "" {
			r.Reason = reason
			return r
		}
		for _, c := range cs {
			if d, ok := DigestFromImageID(c.ImageID); ok {
				r.Digests = append(r.Digests, d)
			} else {
				r.Unresolved++
			}
		}
		return r
	}
}

// Compose reads the digests of the containers of a docker compose project through the docker CLI. The
// digest is the image's RepoDigest (the registry manifest digest); a locally built image has none and is
// counted unresolved. An executor with no docker (the usual compose executor: no socket is handed to it)
// answers with a reason.
func Compose(run CmdRunner, project string) Func {
	return func(ctx context.Context) Reading {
		r := Reading{Source: "compose"}
		if strings.TrimSpace(project) == "" {
			r.Reason = "no compose project declared (deploy.compose_project in argus-config.yaml)"
			return r
		}
		ctx, cancel := context.WithTimeout(ctx, measureTimeout)
		defer cancel()
		ids, err := run(ctx, "docker", "ps", "--filter", "label=com.docker.compose.project="+project, "--format", "{{.ID}}")
		if err != nil {
			r.Reason = "docker is not available to this executor: " + oneLine(err.Error())
			return r
		}
		containers := strings.Fields(string(ids))
		if len(containers) == 0 {
			r.Reason = fmt.Sprintf("no running container found for compose project %s", project)
			return r
		}
		out, err := run(ctx, "docker", append([]string{"inspect", "--format", "{{.Image}}"}, containers...)...)
		if err != nil {
			r.Reason = "docker inspect failed: " + oneLine(err.Error())
			return r
		}
		for _, img := range strings.Fields(string(out)) {
			raw, err := run(ctx, "docker", "image", "inspect", "--format", "{{json .RepoDigests}}", img)
			if err != nil {
				r.Unresolved++
				continue
			}
			var repoDigests []string
			if json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &repoDigests) != nil {
				r.Unresolved++
				continue
			}
			got := false
			for _, rd := range repoDigests {
				if d, ok := DigestFromImageID(rd); ok {
					r.Digests = append(r.Digests, d)
					got = true
				}
			}
			if !got {
				r.Unresolved++
			}
		}
		return r
	}
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// SystemConfig says where this executor's SUT runs. Everything is injectable so no test touches a
// cluster or a docker daemon.
type SystemConfig struct {
	Tier           string                             // compose | k3d | managed
	Namespace      string                             // the SUT's namespace (ARGUS_SUT_NAMESPACE) on k8s tiers
	ComposeProject func() string                      // the SUT's compose project, read lazily from argus-config.yaml
	NewClient      func() (*envcapture.Client, error) // nil: the in-cluster service account
	Run            CmdRunner                          // nil: os/exec
}

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// System picks the measurer for the executor's tier. An unknown tier is a reading with a reason.
func System(cfg SystemConfig) Func {
	switch cfg.Tier {
	case "compose":
		run := cfg.Run
		if run == nil {
			run = execRun
		}
		return func(ctx context.Context) Reading {
			project := ""
			if cfg.ComposeProject != nil {
				project = cfg.ComposeProject()
			}
			return Compose(run, project)(ctx)
		}
	case "k3d", "managed":
		return func(ctx context.Context) Reading {
			if cfg.Namespace == "" {
				return Reading{Source: "k8s", Reason: "no SUT namespace declared (ARGUS_SUT_NAMESPACE is unset — render-k8s with --sut-namespace)"}
			}
			newClient := cfg.NewClient
			if newClient == nil {
				newClient = envcapture.NewInClusterClient
			}
			cl, err := newClient()
			if err != nil {
				return Reading{Source: "k8s", Reason: "no in-cluster Kubernetes credentials: " + oneLine(err.Error())}
			}
			return K8s(cl, cfg.Namespace)(ctx)
		}
	}
	return func(context.Context) Reading {
		return Reading{Reason: fmt.Sprintf("unknown deployment tier %q: the executor does not know where the SUT runs", cfg.Tier)}
	}
}
