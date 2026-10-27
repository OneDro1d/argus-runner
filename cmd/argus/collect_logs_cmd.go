package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/envcapture"
	"github.com/OneDro1d/argus-runner/internal/obsquery"
	"github.com/OneDro1d/argus-runner/internal/sutlogs"
)

// collect_logs_cmd.go — `argus collect-logs`: the long-running process of the Deployment
// render-k8s --collect-sut-logs --sut-logs-via api renders. It follows the pod logs of the SUT namespace
// over the Kubernetes API and ships them to the executor's Loki with promtail's labels. All the work is in
// internal/sutlogs; this file only turns flags and the environment the renderer sets into its options.

// collectLogsPlan is everything cmdCollectLogs derives before it touches the cluster.
type collectLogsPlan struct {
	Namespace string
	SelfPod   string
	SelfNS    string
	Rules     sutlogs.Rules
	Loki      *obsquery.Loki
}

func sameLokiBase(a, b string) bool {
	norm := func(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }
	return norm(a) == norm(b)
}

// planCollectLogs reads the config (WITHOUT resolving the SUT's own ${VAR}s: this process is given none
// of them) and the environment, and refuses what it cannot run with. The Loki it ships to is the one
// the executor of the same obs mode reads (toolcore.lokiFor): --loki, --loki-tenant, and the configured
// credential and push endpoint only when --loki IS the configured Loki.
func planCollectLogs(cf *commonFlags) (*collectLogsPlan, error) {
	instance := strings.TrimSpace(os.Getenv("ARGUS_INSTANCE_ID"))
	if instance == "" {
		return nil, fmt.Errorf("ARGUS_INSTANCE_ID is not set: it is the argus_instance label of every line, and render-k8s sets it")
	}
	ns := strings.TrimSpace(os.Getenv("ARGUS_SUT_NAMESPACE"))
	if ns == "" {
		return nil, fmt.Errorf("ARGUS_SUT_NAMESPACE is not set: it names the namespace whose pod logs are collected, and render-k8s sets it")
	}
	if err := sharedTenantGuard(cf.obsMode, cf.lokiTenant); err != nil {
		return nil, err
	}
	base := strings.TrimSpace(cf.loki)
	if base == "" {
		return nil, fmt.Errorf("--loki is empty: there is no Loki to ship the logs to")
	}
	c, err := config.ParseUnresolved(cf.configPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	l := &obsquery.Loki{BaseURL: base, Tenant: cf.lokiTenant}
	if cfgURL := c.LokiURLConfigured(); cfgURL != "" && sameLokiBase(base, cfgURL) {
		cred, cerr := c.ResolvedLokiCredential()
		if cerr != nil {
			return nil, cerr
		}
		l.Credential = cred
		l.PushURL = c.LokiPushURLConfigured()
	}
	return &collectLogsPlan{
		Namespace: ns,
		SelfPod:   os.Getenv("POD_NAME"),
		SelfNS:    os.Getenv("POD_NAMESPACE"),
		Rules: sutlogs.Rules{
			Instance:         instance,
			Project:          c.ProjectLabel(),
			Cluster:          os.Getenv("ARGUS_CLUSTER"),
			Format:           c.LogFormat(),
			LevelField:       c.LevelField(),
			CorrelationField: c.CorrelationField(),
			SagaEventField:   c.SagaEventField(),
			SagaMarkers:      c.SagaEventValues(),
		},
		Loki: l,
	}, nil
}

// collectLogsServiceAccountDir is where the process finds its ServiceAccount token and CA. A variable only
// so a test can point it at a directory that does not exist; production never changes it.
var collectLogsServiceAccountDir = envcapture.DefaultServiceAccountDir

func cmdCollectLogs(cf *commonFlags) int {
	plan, err := planCollectLogs(cf)
	if err != nil {
		return emitErr(exitUsage, "collect-logs: %v", err)
	}
	api, err := envcapture.NewStreamClientFromDir(collectLogsServiceAccountDir)
	if err != nil {
		return emitErr(exitErr, "collect-logs: this process is not in a pod with a ServiceAccount (%v)", err)
	}
	warn := &sutlogs.RateLog{Out: os.Stderr}
	ship := &sutlogs.Shipper{Loki: plan.Loki, Warn: warn}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	warn.Emit("info", fmt.Sprintf("collecting the pod logs of namespace %q for instance %q (API, no host mount)", plan.Namespace, plan.Rules.Instance))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ship.Run(ctx)
	}()
	sutlogs.Run(ctx, sutlogs.Options{
		API: api, Namespace: plan.Namespace, SelfPod: plan.SelfPod, SelfNamespace: plan.SelfNS,
		Rules: plan.Rules, Sink: ship, Warn: warn,
	})
	wg.Wait()
	return exitOK
}
