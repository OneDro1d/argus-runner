package main

// upgrade_cmd.go — `argus upgrade --instance <id>` (onboarding review items 11 and 22).
//
// Picking up a newer kit used to mean re-running onboarding: the control plane's "Update" changes the
// executor IMAGE (internal/runner/updateguard.go) and nothing else, so a newer render's ConfigMap,
// env, RBAC, annotations (the pull-secret-registries one included), probes... never reached a running
// instance. This renders the instance fresh from the SAME inputs render-k8s uses (renderK8sInput, one
// function, not a copy), compares it with the namespace, shows the difference per object, and — only
// with --apply — creates what the newer kit adds and patches the objects that differ.
//
// DECISIONS (each is pinned by a test in upgrade_cmd_test.go / upgrade_cmd_fixes_test.go):
//   - Dry run is the default. Without --apply every kubectl call is a read (`get`, `config view`).
//   - Secrets are never created, patched, compared or given to `kubectl apply` (which copies Secret
//     data into last-applied-configuration), and no Secret is ever READ: the only question asked about
//     one is whether it exists (`get secret <name> -o name`, names only). A Secret the render has or a
//     workload names and the cluster lacks is reported, with the sentence that the user must create it
//     (`argus secrets …`), and every workload that needs it is left unpatched (exit 4).
//   - An object the render has and the cluster lacks is CREATED with `kubectl create -f -` (never
//     `apply`), in dependency order and before any workload is patched: a Deployment that gains a
//     volume from a ConfigMap the cluster does not have would otherwise never start a pod. A create
//     that fails stops the run exactly like a failed patch. A kind the API server does not serve (a
//     PodMonitor without the Prometheus Operator) is reported and skipped.
//   - The executor IMAGE is left alone: it is neither compared nor sent in the patch. The control
//     plane's Update owns image changes and its guard (updateguard.go); the render is made with the
//     RUNNING image, so the pull-secret-registries annotation is computed for the registry that runs.
//     When --image names a different image the output says it was left alone.
//   - The executor Deployment's spec.replicas is left alone: the executor scales its own Deployment
//     (idle 1 / run 3), and the render's genesis 1 would undo that.
//   - The identity mount follows the LIVE Deployment: if it mounts exec-identity, the render is made
//     as if a key were supplied (a placeholder that only ever reaches the skipped Secret), so a shell
//     without ARGUS_IDENTITY_KEY_B64 cannot point ARGUS_IDENTITY_PATH back at the results volume and
//     have the pods mint a second machine identity.
//   - An identity change is refused (exit 3, nothing written, dry run included): if the render would
//     CHANGE a live value of ARGUS_CP_URL, ARGUS_WORKSPACE_ID, ARGUS_INSTANCE_ID, ARGUS_TIER,
//     ARGUS_CLUSTER, ARGUS_SUT_NAMESPACE, ARGUS_KUBE_CONTEXT_HOST or the Namespace's sut-namespace /
//     tier labels, the shell holds other inputs than the instance was onboarded with, and --apply
//     would repoint the executor. --accept-identity-change turns the refusal off.
//   - upgrade never removes: a field the cluster has and the render no longer sets is not touched.
//   - Refusals (namespace / executor not found, another instance in the cluster, an identity change,
//     render fails) exit exitDenied with a reason and change nothing. Which cluster was asked is
//     printed first, so "not found" can be read against the right API server.
//   - The config loader needs every ${VAR} the config references to be set in the shell. Those values
//     are read to load the config; none is written to the cluster or printed.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/k8srender"
	"github.com/OneDro1d/argus-runner/internal/k8supgrade"
)

const upgradeUsage = `usage: argus upgrade --instance <id> --config <argus-config.yaml> [the flags you gave render-k8s] [--apply] [--accept-identity-change] [--kubeconfig <path>] [--kube-context <ctx>]

  Brings a running instance up to date with a newer kit, in place. It renders the instance fresh with
  the same inputs render-k8s uses, compares that with what runs in namespace argus-inst-<id>, shows the
  difference object by object, and with --apply creates what the newer kit adds and patches the objects
  that differ. The first line of output names the kube context and API server it talks to.

  Pass the SAME flags and environment you onboarded with (--tier, --sut-namespace, --obs, --storage-class,
  --collect-sut-logs, --secrets-env-file, ARGUS_CP_URL, ...): the comparison is against what those
  inputs render today, and a flag you leave out renders as its default. The first run is a dry run;
  read it before you apply.

  --instance <id>     the instance to upgrade (required); the cluster must hold that instance
  --apply             write to the cluster. WITHOUT it this is a DRY RUN: nothing is written, and the
                      output says so
  --accept-identity-change
                      allow the render to CHANGE the instance's identity (see Refuses below); the
                      default is to refuse
  --kubeconfig <path> given to EVERY kubectl call, as in ` + "`argus secrets`" + `; combine with --kube-context
                      (else ARGUS_KUBE_CONTEXT_HOST) to pick a context inside that file

  Creates and patches, never removes. An object the render has and the cluster lacks (a ConfigMap a new
  Deployment mounts, say) is created with ` + "`kubectl create -f -`" + ` before any workload is patched, in
  dependency order; a dry run prints it as "would be created". A field the newer kit no longer sets stays
  as it is. A kind the API server does not serve (a PodMonitor without the Prometheus Operator) is
  reported and skipped. A field the API server will not change on an existing object (a Deployment
  selector, a volume claim's class) is reported as immutable and left alone, and the command then exits 4.
  If a create or a patch fails, the run stops: what was changed, what failed and what was not attempted
  are listed.

  Secrets (the exec-tokens Secret, the executor's credential, and every other): never created, patched,
  compared or printed, and its content is never read; only its existence is checked, by name (` + "`kubectl get secret <name> -o name`" + `). A Secret the render or a workload needs and the
  cluster lacks is named, you create it (` + "`argus secrets`" + `), and every workload that needs it is left
  unpatched (exit 4). No secret value is printed; an env value that comes from a Secret shows as the
  Secret's name, and a URL's user:password is masked.
  The config loader needs every ${VAR} the config references to be set in the shell, so those values ARE
  read by this command to load the config; they are never written or printed.
  Left alone on purpose: the executor image (the control plane's Update owns image changes; the output
  says when this render's image differs) and the executor's replica count (the executor scales itself).
  Refuses, changing nothing (exit 3): the namespace or its executor Deployment is not found, the cluster
  holds a different instance id than --instance, the render fails, or the render would CHANGE a live
  identity value (ARGUS_CP_URL, ARGUS_WORKSPACE_ID, ARGUS_INSTANCE_ID, ARGUS_TIER, ARGUS_CLUSTER,
  ARGUS_SUT_NAMESPACE, ARGUS_KUBE_CONTEXT_HOST, the namespace's sut-namespace and tier labels): pass the
  flags and environment the instance was onboarded with. Adding one the live object lacks is allowed.
  A second run right after a successful --apply reports no differences.
`

// upgradeKindOrder is the order objects are created and patched in: what the workloads depend on first,
// the workloads last, so a new config is in place before a pod that reads it rolls.
var upgradeKindOrder = map[string]int{
	"Namespace": 0, "ServiceAccount": 1, "ClusterRole": 2, "Role": 3, "ClusterRoleBinding": 4, "RoleBinding": 5,
	"ConfigMap": 6, "PersistentVolumeClaim": 7, "ResourceQuota": 8, "NetworkPolicy": 9, "Service": 10,
	"PodMonitor": 11, "DaemonSet": 12, "Deployment": 13,
}

const (
	instanceLabel = "argus.onedroid.ai/instance"
	// identityPlaceholder stands in for ARGUS_IDENTITY_KEY_B64 when the live Deployment mounts the
	// identity Secret. It is not a key and is only ever rendered into the exec-identity Secret, which is skipped.
	identityPlaceholder = "cGxhY2Vob2xkZXI="
)

type upgradeResult struct {
	obj     k8supgrade.Object
	status  string // secret | missing | nokind | unchanged | differs
	desired map[string]any
	changes []k8supgrade.Change
	live    map[string]any // the object as read, for what a patch must carry over from it
	blocked bool           // an immutable field differs
	needs   []string       // why the object is NOT written: what it depends on that is missing
	secret  string         // for status secret: exists | missing | unknown: <why>
}

// upgradeStep is one write: a create or a patch.
type upgradeStep struct {
	r      *upgradeResult
	create bool
}

var errKindNotInstalled = errors.New("kind not installed")

func cmdUpgrade(args []string) int {
	if len(args) > 0 && isHelpToken(args[0]) {
		fmt.Print(upgradeUsage)
		return exitOK
	}
	cf := &commonFlags{}
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cf.bind(fs)
	instance := fs.String("instance", "", "the instance to upgrade (required)")
	apply := fs.Bool("apply", false, "create and patch (default: dry run)")
	acceptIdentity := fs.Bool("accept-identity-change", false, "allow the render to change the instance's identity values")
	kubeconfig := fs.String("kubeconfig", "", "the kubeconfig file kubectl uses; passed to EVERY kubectl call")
	if err := fs.Parse(args); err != nil {
		return emitErr(exitUsage, "upgrade: flag error: %v\n%s", err, upgradeUsage)
	}
	if fs.NArg() > 0 {
		return emitErr(exitUsage, "upgrade: unexpected argument %q\n%s", fs.Arg(0), upgradeUsage)
	}
	id := strings.TrimSpace(*instance)
	if id == "" {
		return emitErr(exitUsage, "upgrade: --instance is required\n%s", upgradeUsage)
	}
	explicitID := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "instance-id" {
			explicitID = true
		}
	})
	if explicitID && cf.instance != id {
		return emitErr(exitUsage, "upgrade: --instance %q and --instance-id %q name different instances", id, cf.instance)
	}
	cf.instance = id
	if cf.configPath == "" {
		return emitErr(exitUsage, "%s", missingConfigMsg("upgrade"))
	}
	if cf.outDir != "" || cf.emitSUTAccessRole {
		return emitErr(exitUsage, "upgrade: --out and --emit-sut-access-role belong to render-k8s; upgrade writes no files and never touches the SUT's namespace")
	}

	sf := secretsCommonFlags{kubeconfig: strings.TrimSpace(*kubeconfig), kubeContext: renderKubeContext(cf)}
	ns := "argus-inst-" + id

	// ---- 0. which cluster ------------------------------------------------------------------------
	fmt.Printf("argus upgrade: cluster: %s\n", upgradeCluster(sf))
	// ---- 1. the cluster must hold this instance -------------------------------------------------
	nsObj, found, err := upgradeGet(sf, "namespace", ns, "")
	if err != nil {
		return emitErr(exitErr, "upgrade: %v", err)
	}
	if !found {
		return emitErr(exitDenied, "upgrade refused: namespace %s not found in the cluster — nothing to upgrade (onboard the instance first)", ns)
	}
	dep, found, err := upgradeGet(sf, "deployment.apps", k8supgrade.ExecutorName, ns)
	if err != nil {
		return emitErr(exitErr, "upgrade: %v", err)
	}
	if !found {
		return emitErr(exitDenied, "upgrade refused: executor Deployment not found in namespace %s — nothing to upgrade", ns)
	}
	liveImage, hasIdentity, liveIDs := inspectExecutor(nsObj, dep)
	if liveImage == "" {
		return emitErr(exitDenied, "upgrade refused: the executor Deployment in %s has no container named %q", ns, k8supgrade.ExecutorName)
	}
	if len(liveIDs) == 0 {
		return emitErr(exitDenied, "upgrade refused: cannot confirm that namespace %s is instance %q (no %s label, no ARGUS_INSTANCE_ID)", ns, id, instanceLabel)
	}
	for _, got := range liveIDs {
		if got != id {
			return emitErr(exitDenied, "upgrade refused: --instance is %q but the cluster's %s says instance %q", id, ns, got)
		}
	}

	// ---- 2. render fresh, from render-k8s's own inputs -------------------------------------------
	rb, _ := renderK8sInput(env(cf), cf, sf.kubeconfig)
	if rb == nil {
		return exitDenied // renderK8sInput already printed the one-line reason
	}
	if rb.podMonitorNote != "" {
		fmt.Fprintln(os.Stderr, rb.podMonitorNote)
	}
	in := rb.in
	renderImage := strings.TrimSpace(in.Image)
	in.Image = liveImage // the image is the control plane's: render with the one that runs
	// No secret input is needed, and none is kept: the Secrets are skipped.
	in.RunnerToken, in.AuthorToken, in.EnrollmentToken, in.SecretEnv, in.IdentityKeyB64 = "", "", "", nil, ""
	if hasIdentity {
		in.IdentityKeyB64 = identityPlaceholder
	}
	execYAML, err := k8srender.RenderExecutor(in)
	if err != nil {
		return emitErr(exitDenied, "upgrade refused: render executor: %v", err)
	}
	obsYAML, err := k8srender.RenderObs(in, rb.c)
	if err != nil {
		return emitErr(exitDenied, "upgrade refused: render obs: %v", err)
	}
	objs, err := k8supgrade.Parse(execYAML, obsYAML)
	if err != nil {
		return emitErr(exitDenied, "upgrade refused: %v", err)
	}

	// ---- 3. compare ---------------------------------------------------------------------------------
	secretState := map[string]string{} // name -> exists | missing | unknown: <why>
	checkSecret := func(name string) string {
		if st, ok := secretState[name]; ok {
			return st
		}
		st := "exists"
		if _, serr := runKubectl(nil, sf.kubectlArgs("get", "secret", name, "-n", ns, "-o", "name")...); serr != nil {
			if k8supgrade.IsAPINotFound(serr.Error()) {
				st = "missing"
			} else {
				st = "unknown: " + k8supgrade.MaskCreds(serr.Error())
			}
		}
		secretState[name] = st
		return st
	}
	var results []*upgradeResult
	for _, o := range objs {
		r := &upgradeResult{obj: o}
		results = append(results, r)
		if o.IsSecret() {
			r.status = "secret"
			continue
		}
		live, found, err := upgradeGet(sf, o.Resource(), o.Name, o.Namespace)
		if errors.Is(err, errKindNotInstalled) {
			r.status = "nokind"
			continue
		}
		if err != nil {
			return emitErr(exitErr, "upgrade: %v", err)
		}
		r.desired = k8supgrade.Prepare(o)
		if !found {
			r.status = "missing"
			continue
		}
		r.live = live
		r.changes = k8supgrade.Diff(o, r.desired, live)
		r.status = "unchanged"
		if len(r.changes) > 0 {
			r.status = "differs"
			for _, c := range r.changes {
				if c.Immutable {
					r.blocked = true
				}
			}
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		return upgradeKindOrder[results[i].obj.Kind] < upgradeKindOrder[results[j].obj.Kind]
	})
	byRef := map[k8supgrade.Ref]*upgradeResult{}
	for _, r := range results {
		byRef[k8supgrade.Ref{Kind: r.obj.Kind, Name: r.obj.Name}] = r
	}
	for _, r := range results {
		if r.obj.IsSecret() {
			r.secret = checkSecret(r.obj.Name)
		}
	}
	// A workload that is about to be written must not need something that is not there.
	for _, r := range results {
		if !r.obj.IsWorkload() || (r.status != "differs" && r.status != "missing") || r.blocked {
			continue
		}
		for _, ref := range k8supgrade.WorkloadRefs(r.desired) {
			switch {
			case ref.Kind == "Secret":
				switch st := checkSecret(ref.Name); {
				case st == "missing":
					r.needs = append(r.needs, fmt.Sprintf("%s, which does not exist in %s (upgrade never creates a Secret: create it yourself, with `argus secrets …`)", ref, ns))
				case st != "exists":
					r.needs = append(r.needs, fmt.Sprintf("%s, which could not be confirmed (%s)", ref, st))
				}
			default:
				if dep := byRef[ref]; dep != nil && dep.status == "nokind" {
					r.needs = append(r.needs, fmt.Sprintf("%s, a kind this cluster does not serve", ref))
				}
			}
		}
	}

	// ---- 4. identity: refuse a render that repoints the instance ---------------------------------------
	if !*acceptIdentity {
		var lines []string
		for _, r := range results {
			if r.status != "differs" {
				continue
			}
			for _, ic := range k8supgrade.IdentityChanges(r.obj, r.changes) {
				lines = append(lines, fmt.Sprintf("  %s %s: live %q, rendered %q", r.obj.Ref(), ic.Field, k8supgrade.MaskCreds(ic.Live), k8supgrade.MaskCreds(ic.Desired)))
			}
		}
		if len(lines) > 0 {
			fmt.Printf("upgrade refused: this render would CHANGE what identifies instance %s (nothing was written):\n%s\n", id, strings.Join(lines, "\n"))
			fmt.Println("The flags or environment in this shell are not the ones the instance was onboarded with. Pass the flags and environment the instance was onboarded with")
			fmt.Println("(--tier, --sut-namespace, ARGUS_CP_URL, ARGUS_WORKSPACE_ID, ARGUS_KUBE_CONTEXT_HOST ...) and run again. If the change is really intended, pass --accept-identity-change.")
			return exitDenied
		}
	}

	// ---- 5. the plan: creates of everything but workloads first, then in dependency order ----------------
	var steps []upgradeStep
	for _, r := range results {
		if r.status == "missing" && !r.obj.IsWorkload() {
			steps = append(steps, upgradeStep{r, true})
		}
	}
	for _, r := range results {
		switch {
		case r.status == "differs" && !r.blocked && len(r.needs) == 0:
			steps = append(steps, upgradeStep{r, false})
		case r.status == "missing" && r.obj.IsWorkload() && len(r.needs) == 0:
			steps = append(steps, upgradeStep{r, true})
		}
	}

	// ---- 6. report ------------------------------------------------------------------------------------
	mode := "DRY RUN — nothing is written to the cluster; pass --apply to create and patch"
	if *apply {
		mode = "APPLY — creating and patching (Secrets are never touched)"
	}
	fmt.Printf("argus upgrade: instance %s, namespace %s\nmode: %s\n\n", id, ns, mode)

	var differ, unchanged, secrets, missingSecrets, toCreate, nokind, immutable, needy int
	for _, r := range results {
		ref := r.obj.Ref()
		switch r.status {
		case "secret":
			secrets++
			switch {
			case r.secret == "exists":
				fmt.Printf("%s  SKIPPED — its content is never read, compared, patched or created by upgrade; only its existence is checked, by name (`kubectl get secret <name> -o name`), and it exists\n", ref)
			case r.secret == "missing":
				missingSecrets++
				fmt.Printf("%s  MISSING from the cluster — upgrade never creates a Secret or reads its content, and only checks that it exists, by name (`kubectl get secret <name> -o name`); create it yourself (`argus secrets …`)\n", ref)
			default:
				missingSecrets++
				fmt.Printf("%s  SKIPPED — its content is never read, compared, patched or created by upgrade; only its existence is checked, by name (`kubectl get secret <name> -o name`), and that could not be confirmed (%s)\n", ref, r.secret)
			}
		case "nokind":
			nokind++
			fmt.Printf("%s  kind not installed in this cluster — skipped\n", ref)
		case "missing":
			verb := "would be created"
			if *apply {
				verb = "will be created"
			}
			if len(r.needs) > 0 {
				verb = "NOT created"
			}
			fmt.Printf("%s  not in the cluster — %s\n", ref, verb)
			if len(r.needs) > 0 {
				needy++
			} else {
				toCreate++
			}
		case "unchanged":
			unchanged++
			fmt.Printf("%s  unchanged (left alone)\n", ref)
		case "differs":
			differ++
			fmt.Printf("%s  DIFFERS (%d field%s)\n", ref, len(r.changes), plural(len(r.changes)))
			for _, l := range k8supgrade.FormatChanges(r.changes) {
				fmt.Println(l)
			}
			if r.blocked {
				immutable++
				fmt.Printf("  => left alone: a field above is immutable on an existing object; only re-onboarding can change it\n")
			} else if len(r.needs) > 0 {
				needy++
			} else if note := k8supgrade.RestartNote(r.obj, r.changes, *apply); note != "" {
				fmt.Println(note)
			}
		}
		for _, n := range r.needs {
			fmt.Printf("  => NOT patched or created: it needs %s\n", n)
		}
	}

	// ---- 7. apply -------------------------------------------------------------------------------------
	var done, failedStep, notAttempted int
	var failure string
	if *apply && len(steps) > 0 {
		fmt.Printf("\napplying %d step%s (creates first, workloads last):\n", len(steps), plural(len(steps)))
		for _, st := range steps {
			ref := st.r.obj.Ref()
			verb := "patch"
			if st.create {
				verb = "create"
			}
			if failure != "" {
				notAttempted++
				fmt.Printf("  => NOT ATTEMPTED: %s %s (an earlier step failed)\n", verb, ref)
				continue
			}
			var aerr error
			if st.create {
				var body []byte
				body, aerr = k8supgrade.CreateBody(st.r.desired)
				if aerr == nil {
					_, aerr = runKubectl(strings.NewReader(string(body)), sf.kubectlArgs("create", "-f", "-")...)
				}
			} else {
				var typ string
				var body []byte
				if k8supgrade.PortsNeedAWholeListPatch(st.r.obj, st.r.changes) {
					// A Service port whose number changed while its name stayed: the list is replaced whole.
					typ, body, aerr = k8supgrade.ServicePortsPatch(st.r.obj, st.r.desired, st.r.live)
				} else {
					// Aligned to live: the patch must not change what the comparison called unchanged.
					typ, body, aerr = k8supgrade.PatchFor(st.r.obj, st.r.desired, st.r.live)
				}
				if aerr == nil {
					pargs := []string{"patch", st.r.obj.Resource(), st.r.obj.Name}
					if st.r.obj.Namespace != "" {
						pargs = append(pargs, "-n", st.r.obj.Namespace)
					}
					pargs = append(pargs, "--type="+typ, "--patch-file=/dev/stdin")
					_, aerr = runKubectl(strings.NewReader(string(body)), sf.kubectlArgs(pargs...)...)
				}
			}
			if aerr != nil {
				failedStep++
				failure = fmt.Sprintf("%s %s failed: %s", verb, ref, k8supgrade.MaskCreds(aerr.Error()))
				fmt.Printf("  => FAILED %s %s: %s\n", verb, ref, k8supgrade.MaskCreds(aerr.Error()))
				continue
			}
			done++
			if st.create {
				fmt.Printf("  => created %s\n", ref)
			} else if k8supgrade.RestartNote(st.r.obj, st.r.changes, true) != "" {
				fmt.Printf("  => patched %s (its pod template changed: its pods are being replaced)\n", ref)
			} else {
				fmt.Printf("  => patched %s\n", ref)
			}
		}
	}

	fmt.Println()
	if renderImage != "" && renderImage != liveImage {
		fmt.Printf("executor image: left alone — running %s, this render asks for %s. The control plane's Update owns image changes.\n", liveImage, renderImage)
	} else {
		fmt.Printf("executor image: left alone (%s) — the control plane's Update owns image changes.\n", liveImage)
	}
	fmt.Printf("executor replicas: left alone — the executor scales its own Deployment.\n")
	fmt.Printf("summary: %d differ, %d to create, %d unchanged, %d Secret%s skipped (%d missing), %d kind%s not installed, %d immutable, %d not written for a missing dependency.\n",
		differ, toCreate, unchanged, secrets, plural(secrets), missingSecrets, nokind, plural(nokind), immutable, needy)

	switch {
	case failure != "":
		fmt.Printf("apply stopped: %s (%d step%s done before it; %d not attempted)\n", failure, done, plural(done), notAttempted)
		return exitErr
	case differ == 0 && toCreate == 0 && needy == 0 && missingSecrets == 0:
		fmt.Println("no differences: this instance already matches what the render produces today.")
		if !*apply {
			fmt.Println("DRY RUN: no changes were made.")
		}
		return exitOK
	}
	if !*apply {
		fmt.Println("DRY RUN: no changes were made. Re-run with --apply to create what is missing and patch the objects marked DIFFERS.")
	} else {
		fmt.Printf("apply: %d step%s done (%d created or patched); the rest of the %d objects were left alone (unchanged, immutable, Secret, skipped or not written).\n", len(steps), plural(len(steps)), done, len(results))
	}
	if needy > 0 {
		fmt.Printf("%d object%s NOT written because something it needs is missing (above) — exit 4.\n", needy, plural(needy))
		return exitFailed
	}
	if *apply && immutable > 0 {
		fmt.Printf("%d object%s could not be brought up to date (immutable fields) — exit 4.\n", immutable, plural(immutable))
		return exitFailed
	}
	return exitOK
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// upgradeCluster names the kube context and API server this run's kubectl talks to, from the read-only
// `kubectl config view --minify` carrying --kubeconfig/--context like every other call. A failure to
// say is printed, not fatal: the next call surfaces the real problem with kubectl's own text.
func upgradeCluster(sf secretsCommonFlags) string {
	out, err := runKubectl(nil, sf.kubectlArgs("config", "view", "--minify", "-o", "json")...)
	if err != nil {
		return "could not be determined (" + k8supgrade.MaskCreds(err.Error()) + ")"
	}
	var cfg struct {
		Current  string `json:"current-context"`
		Clusters []struct {
			Cluster struct {
				Server string `json:"server"`
			} `json:"cluster"`
		} `json:"clusters"`
	}
	if jerr := json.Unmarshal([]byte(out), &cfg); jerr != nil {
		return "could not be determined (unreadable kubectl config view output)"
	}
	server := "(none)"
	if len(cfg.Clusters) > 0 && cfg.Clusters[0].Cluster.Server != "" {
		server = k8supgrade.MaskCreds(cfg.Clusters[0].Cluster.Server)
	}
	ctx := cfg.Current
	if ctx == "" {
		ctx = "(none)"
	}
	return fmt.Sprintf("kube context %s, API server %s", ctx, server)
}

// upgradeGet reads one object with `kubectl get -o json`. Never called for a Secret. (nil, false, nil)
// means the API server said NotFound — its own `Error from server (NotFound)`, nothing looser: a
// missing kubectl binary, a missing context or any other failure is an error with its own text.
// errKindNotInstalled means the API server does not serve that resource type at all.
func upgradeGet(sf secretsCommonFlags, resource, name, namespace string) (map[string]any, bool, error) {
	a := []string{"get", resource, name}
	if namespace != "" {
		a = append(a, "-n", namespace)
	}
	a = append(a, "-o", "json")
	out, err := runKubectl(nil, sf.kubectlArgs(a...)...)
	if err != nil {
		switch {
		case k8supgrade.IsAPINotFound(err.Error()):
			return nil, false, nil
		case k8supgrade.IsKindNotInstalled(err.Error()):
			return nil, false, errKindNotInstalled
		}
		return nil, false, err
	}
	var m map[string]any
	if jerr := json.Unmarshal([]byte(out), &m); jerr != nil {
		return nil, false, fmt.Errorf("parse kubectl's JSON for %s/%s: %w", resource, name, jerr)
	}
	return m, true, nil
}

// inspectExecutor reads, from the live Namespace and executor Deployment: the running executor image,
// whether the pod mounts the exec-identity volume, and every instance id the cluster states.
func inspectExecutor(nsObj, dep map[string]any) (image string, hasIdentity bool, ids []string) {
	addLabel := func(obj map[string]any) {
		if meta, ok := obj["metadata"].(map[string]any); ok {
			if labels, ok := meta["labels"].(map[string]any); ok {
				if v, ok := labels[instanceLabel].(string); ok && v != "" {
					ids = append(ids, v)
				}
			}
		}
	}
	addLabel(nsObj)
	addLabel(dep)
	spec, _ := dep["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	pod, _ := tpl["spec"].(map[string]any)
	if vols, ok := pod["volumes"].([]any); ok {
		for _, v := range vols {
			if vm, ok := v.(map[string]any); ok && vm["name"] == "identity" {
				hasIdentity = true
			}
		}
	}
	if cs, ok := pod["containers"].([]any); ok {
		for _, c := range cs {
			cm, _ := c.(map[string]any)
			if cm == nil || cm["name"] != k8supgrade.ExecutorName {
				continue
			}
			image, _ = cm["image"].(string)
			if envs, ok := cm["env"].([]any); ok {
				for _, e := range envs {
					if em, ok := e.(map[string]any); ok && em["name"] == "ARGUS_INSTANCE_ID" {
						if v, ok := em["value"].(string); ok && v != "" {
							ids = append(ids, v)
						}
					}
				}
			}
		}
	}
	return image, hasIdentity, ids
}
