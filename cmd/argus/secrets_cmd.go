package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/envname"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// secrets_cmd.go — P1 #9 (tester 2026-09-27, msgbus): "Secrets after enrollment."
//
// `render-k8s --secrets-env-file` (main.go's cmdRenderK8s, ~line 2821) is the ONLY place a SUT
// ${VAR} value has ever reached the exec-tokens Secret: it is folded into `in.SecretEnv` and
// rendered once, at render time (internal/k8srender/k8srender.go:101-107, secretEnvBlock at
// k8srender.go:1246). Adding a scenario credential — or rotating one the SUT changed — after the
// instance was created had no supported path; the tester's own note says it meant
// `kubectl edit secret exec-tokens` by hand, with `apply` a live temptation. `apply` is refused
// project-wide for this exact Secret (docs/DEPLOY-ARGUS.md §4.3, "the single most consequential
// line on this page") because it copies the Secret's data into the
// `kubectl.kubernetes.io/last-applied-configuration` annotation — a second, unencrypted copy of
// every value, readable by anything that can read the object once.
//
// `argus secrets set` is the supported path. It patches ONE key with
// `kubectl patch --type=merge --patch-file=/dev/stdin`: `patch` never touches
// last-applied-configuration (only `apply` maintains that annotation), and `--patch-file=/dev/stdin`
// keeps the value off argv — the patch FILE NAME is a literal path, `/dev/stdin`; the value itself
// travels only over the child process's stdin pipe, which appears in neither `ps`, nor bash
// history, nor this process's own recorded argv.
//
// `argus secrets list` prints Secret KEY NAMES only, via `kubectl get secret … -o json` parsed in
// Go — never a jsonpath/go-template expression run directly against a Secret. A go-template that
// errors on a Secret makes kubectl print "raw data was:" followed by the ENTIRE object, values
// included (measured 2026-09-10, memstore-prod: a `len` on a missing annotation key hit this and
// leaked every Secret in a namespace to a transcript). `-o json` has no template to fail; this
// command's own Go code is the only thing that ever reads `.data`, and it never re-prints a value.
//
// PRE-AUTH, dispatched in main.go's dispatch() alongside `render-k8s`/`preflight`/`router`/`hub`:
// it presents no Argus token. Its authority is whatever kubeconfig/context the caller's kubectl
// already has — exactly like preflight's own kubectl probes (cmd/argus/preflight.go).
//
// secretRef (the executor reading a Secret from the SUT's own namespace, at run time, via a
// RoleBinding the SUT grants — item 9's second ask) is NOT implemented. The executor's ONE
// in-cluster Kubernetes API use today is UC069 (self-delete its own pod) + UC196 (self-scale its
// own Deployment): get/delete on `pods` and get/patch on `deployments`/`deployments/scale`, IN ITS
// OWN NAMESPACE ONLY (internal/k8srender/k8srender.go:802-814, the `argus-executor-selfdelete`
// Role). There is no generic Kubernetes client in the executor (no client-go in go.mod), no RBAC
// for `get secrets` anywhere, and no code path that would read a cross-namespace RoleBinding if one
// existed. Building secretRef for real needs a Secret-reading client, cross-namespace RBAC plumbed
// through render-k8s AND the SUT's own manifests, and a scenario/config field naming which
// namespace+key+Secret to read — a real new capability, not a contained change, against this item's
// budget. So it is documented loudly instead of half-built, here and in docs/DEPLOY-ARGUS.md §4.5
// and skills/scenario-author/SKILL.md: a value `secrets set` writes is a COPY, and a copy goes
// stale SILENTLY when the SUT rotates the source — nothing detects the drift. Re-run
// `argus secrets set` after every rotation.

const secretsUsage = `usage: argus secrets set --key <VAR> --namespace <ns> [--secret-name exec-tokens] [--kube-context <ctx>] [--kubeconfig <path>] [--restart]
       argus secrets list --namespace <ns> [--secret-name exec-tokens] [--kube-context <ctx>] [--kubeconfig <path>]
       argus secrets migrate --namespace <ns> [--secret-name exec-tokens] [--kube-context <ctx>] [--kubeconfig <path>] [--drop-old] [--restart]

  --kubeconfig <path>
                Every kubectl call these commands make (get, patch, replace, rollout restart) gets
                --kubeconfig <path>, and so does the restart command they print. For an in-cluster
                tester whose kubeconfig is not the default (e.g. ~/.config/argus/<app>.kubeconfig):
                ` + "`export KUBECONFIG=...`" + ` does not persist between an agent's separate shell calls and
                silently falls back to the default context. Combine with --kube-context to pick a
                context inside that file.

  secrets set   Patches ONE key of the instance's Secret from a value read on STDIN ONLY — never a
                flag, never an env var named on the command line, never echoed, never logged.
                  echo -n "$VALUE" | argus secrets set --key DB_PASSWORD --namespace argus-inst-<id>
                Uses ` + "`kubectl patch --type=merge --patch-file=/dev/stdin`" + ` — never ` + "`apply`" + `: apply copies
                the whole Secret, values included, into the kubectl.kubernetes.io/last-applied-configuration
                annotation (docs/DEPLOY-ARGUS.md §4.3). patch touches only the named key and never
                writes that annotation.
                ⚠ The executor reads this Secret only at pod start (envFrom: secretRef,
                internal/k8srender/k8srender.go:717) — a patched key has NO EFFECT on a running pod.
                --restart also runs ` + "`kubectl rollout restart deployment/executor -n <namespace>`" + `;
                without it, the exact command is printed (and always in the JSON result — never
                just logged and lost).

  secrets list  Prints the Secret's key NAMES only (never values) — cheap, one ` + "`kubectl get -o json`" + `.

  secrets migrate
                Copies the executor secret key IN PLACE, server-side: copies
                ` + "`" + migrateFromKey + "`" + ` to ` + "`" + migrateToKey + "`" + ` on the instance's exec-tokens Secret (msgbus
                tester follow-up item 3, 2026-09-28). ` + "`" + migrateFromKey + "`" + ` is what every executor
                rendered by <=0.3.39 still carries; a rendered-by-0.3.40+ instance already carries
                BOTH keys and needs no migration (idempotent: already-migrated exits 0 and says so).
                The value is read via ` + "`kubectl get -o json`" + `, copied IN MEMORY, and written back with
                ` + "`kubectl replace -f -`" + ` on stdin — NEVER ` + "`kubectl apply`" + ` (apply copies the Secret's
                data into the kubectl.kubernetes.io/last-applied-configuration annotation, a second
                unencrypted copy of every value; this command also drops that annotation if replace
                found one already there). The value is never printed, never written to a file, and
                never appears in argv.
                If ` + "`" + migrateToKey + "`" + ` already exists with a DIFFERENT value than ` + "`" + migrateFromKey + "`" + `, this
                refuses rather than overwrite either.
                The OLD key is KEPT by default: an executor older than 0.3.40 reads only the old
                name, and a 0.3.40+ executor reads the new one silently when both are set.
                --drop-old also deletes the old key — only once the executor runs 0.3.40 or later.
                ⚠ Same as ` + "`secrets set`" + `: envFrom: secretRef only sets a pod's environment at
                start, so a migrated key has NO EFFECT on an already-running pod until it restarts —
                --restart also runs the rollout restart; without it, the command is printed instead.
                ⛔ The ` + "`" + migrateFromKey + "`" + ` alias stays readable (internal/envname.Lookup, one
                deprecation warning) for every 0.3.x release and is removed no earlier than v0.4.0 —
                after that release, an executor that still needs the old key (never migrated) loses
                its credential. Migrate before upgrading past the release that drops the alias.

  LIMIT (no secretRef): the executor cannot read a Kubernetes Secret in the SUT's own namespace at
  run time, even when the SUT grants it a RoleBinding for exactly that (the tester's
  deploy/k8s/55-argus-test-access.yaml does this and the executor uses none of it) — it has no
  generic Secret-reading client and no RBAC for ` + "`get secrets`" + ` anywhere; its one in-cluster grant is
  get/delete on its OWN pods and get/patch on its OWN deployments/scale
  (internal/k8srender/k8srender.go:802-814). A value set here is a COPY: it goes stale SILENTLY
  when the SUT rotates the source. Re-run ` + "`argus secrets set`" + ` after every rotation.
`

// migrateFromKey/migrateToKey are the ONE rename `secrets migrate` performs — the same pair
// internal/envname documents and internal/k8srender/k8srender.go's exec-tokens render dual-writes
// from 0.3.40 on. This is a FIXED, purpose-built migration for that one pair, not a generic
// renamer: there is exactly one rename in flight this release.
const (
	migrateFromKey = envname.ExecutorSecretDeprecated
	migrateToKey   = envname.ExecutorSecret
)

// cmdSecrets dispatches `secrets set|list`. PRE-AUTH: see main.go's dispatch() for why.
func cmdSecrets(args []string) int {
	if len(args) == 0 {
		fmt.Print(secretsUsage)
		return exitUsage
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		fmt.Print(secretsUsage)
		return exitOK
	case "set":
		return cmdSecretsSet(args[1:])
	case "list":
		return cmdSecretsList(args[1:])
	case "migrate":
		return cmdSecretsMigrate(args[1:])
	default:
		return emitErr(exitUsage, "secrets: unknown subcommand %q (set|list|migrate)\n%s", args[0], secretsUsage)
	}
}

// secretsCommonFlags is shared by `secrets set` and `secrets list`.
type secretsCommonFlags struct {
	namespace   string
	secretName  string
	kubeContext string
	kubeconfig  string
}

func (s *secretsCommonFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&s.namespace, "namespace", "", "the instance namespace the Secret lives in (required)")
	fs.StringVar(&s.secretName, "secret-name", "exec-tokens", "the Secret to act on (default matches what render-k8s writes SUT ${VAR}s into — internal/k8srender/k8srender.go:546)")
	fs.StringVar(&s.kubeContext, "kube-context", "", "the kubectl context to act against (else kubectl's current context)")
	fs.StringVar(&s.kubeconfig, "kubeconfig", "", "the kubeconfig file kubectl uses (else KUBECONFIG / ~/.kube/config); passed to EVERY kubectl call — for an in-cluster tester whose kubeconfig is not the default")
}

// kubectlPrefix is the global kubectl flags this command was given, in the order kubectl reads them
// (before the verb): --kubeconfig, then --context. Empty when neither was given.
func (s *secretsCommonFlags) kubectlPrefix() []string {
	var p []string
	if s.kubeconfig != "" {
		p = append(p, "--kubeconfig", s.kubeconfig)
	}
	if s.kubeContext != "" {
		p = append(p, "--context", s.kubeContext)
	}
	return p
}

// kubectlArgs prepends --kubeconfig and --context when given. kubectl reads both before the verb.
func (s *secretsCommonFlags) kubectlArgs(rest ...string) []string {
	return append(s.kubectlPrefix(), rest...)
}

// restartCommand is the rollout-restart command to print, carrying the same --kubeconfig/--context this
// command's own kubectl calls used — a printed command that dropped them would act on a different cluster.
func (s *secretsCommonFlags) restartCommand() string {
	parts := append([]string{"kubectl"}, s.kubectlPrefix()...)
	parts = append(parts, "rollout", "restart", "deployment/executor", "-n", s.namespace)
	return strings.Join(parts, " ")
}

// runKubectl shells out to the REAL kubectl on PATH (fakeable only by putting a different
// `kubectl` earlier on PATH, exactly as preflight.go's execProbes.run does — never an injectable
// Go function var, so a test that fakes this exercises the very same subprocess invocation
// production does).
func runKubectl(stdin io.Reader, args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// kubectl ran and refused; its own message is more useful than ours (preflight.go's rule).
			return "", fmt.Errorf("kubectl %s: %s", strings.Join(args, " "), strings.TrimSpace(errOut.String()))
		}
		return "", fmt.Errorf("could not execute kubectl: %w", err)
	}
	return out.String(), nil
}

// kubectlNotExecutable reports whether err (from runKubectl) means kubectl could not even be started
// on THIS machine (not on PATH), as opposed to kubectl running and the cluster or API refusing.
// AC-D57 (#391): the executor image ships no kubectl, so render-k8s must be able to tell the two apart.
func kubectlNotExecutable(err error) bool {
	return err != nil && errors.Is(err, exec.ErrNotFound)
}

func cmdSecretsSet(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "-help", "--help", "help":
			fmt.Print(secretsUsage)
			return exitOK
		}
	}
	fs := flag.NewFlagSet("secrets set", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var sf secretsCommonFlags
	sf.bind(fs)
	key := fs.String("key", "", "the ${VAR} name this scenario/config references (required)")
	restart := fs.Bool("restart", false, "also run `kubectl rollout restart deployment/executor -n <namespace>` — the executor reads exec-tokens only at pod start, so a patched key otherwise has no effect until the next restart")
	if err := fs.Parse(args); err != nil {
		return emitErr(exitUsage, "secrets set: flag error: %v\n%s", err, secretsUsage)
	}
	if strings.TrimSpace(sf.namespace) == "" {
		return emitErr(exitUsage, "secrets set: --namespace is required\n%s", secretsUsage)
	}
	// A key is an env-var NAME, never a value — the same shape amqp's `url_env` already enforces
	// (internal/scenario/amqpstep.go), reused here so the two credential paths agree on what a name
	// looks like.
	if strings.TrimSpace(*key) == "" || !scenario.ValidURLEnv(*key) {
		return emitErr(exitUsage, "secrets set: --key %q must be a non-empty environment variable NAME matching %s (never a value)", *key, scenario.URLEnvPattern)
	}

	// STDIN ONLY. Never a flag, never an env var NAMED ON THE COMMAND LINE (that name, and the
	// value it resolves to, would sit in argv all the same) — argv is visible in `ps` and in this
	// process's own recorded invocation; stdin is neither.
	raw, rerr := io.ReadAll(os.Stdin)
	if rerr != nil {
		return emitErr(exitErr, "secrets set: read value from stdin: %v", rerr)
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if value == "" {
		return emitErr(exitUsage, "secrets set: no value on stdin — pipe it in, e.g.\n"+
			`  printf '%%s' "$VALUE" | argus secrets set --key %s --namespace %s`, *key, sf.namespace)
	}

	patch, merr := json.Marshal(map[string]any{"stringData": map[string]string{*key: value}})
	if merr != nil {
		return emitErr(exitErr, "secrets set: build patch: %v", merr)
	}
	patchArgs := sf.kubectlArgs("patch", "secret", sf.secretName, "-n", sf.namespace, "--type=merge", "--patch-file=/dev/stdin")
	if _, err := runKubectl(bytes.NewReader(patch), patchArgs...); err != nil {
		return emitErr(exitErr, "secrets set: %v", err)
	}

	restartCmd := sf.restartCommand()
	result := map[string]any{
		"patched":     true,
		"secret_name": sf.secretName, // not "secret": transcript secret-guards redact a field by that name
		"namespace":   sf.namespace,
		"key":         *key,
		"restart_cmd": restartCmd,
		"why_restart": "the executor reads this Secret only at pod start (envFrom: secretRef, internal/k8srender/k8srender.go:717) — a patched key has no effect until the pod restarts",
	}
	if *restart {
		restartArgs := sf.kubectlArgs("rollout", "restart", "deployment/executor", "-n", sf.namespace)
		if _, err := runKubectl(nil, restartArgs...); err != nil {
			result["restarted"] = false
			result["restart_error"] = err.Error()
			emit(result)
			return exitErr
		}
		result["restarted"] = true
	} else {
		result["restarted"] = false
	}
	emit(result)
	return exitOK
}

// secretJSON is the ONLY shape this command reads out of `kubectl get secret -o json` — just
// enough to list key names. It never reads/keeps a value.
type secretJSON struct {
	Data       map[string]string `json:"data"`
	StringData map[string]string `json:"stringData"`
}

func cmdSecretsList(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "-help", "--help", "help":
			fmt.Print(secretsUsage)
			return exitOK
		}
	}
	fs := flag.NewFlagSet("secrets list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var sf secretsCommonFlags
	sf.bind(fs)
	if err := fs.Parse(args); err != nil {
		return emitErr(exitUsage, "secrets list: flag error: %v\n%s", err, secretsUsage)
	}
	if strings.TrimSpace(sf.namespace) == "" {
		return emitErr(exitUsage, "secrets list: --namespace is required\n%s", secretsUsage)
	}
	// -o json, NEVER a jsonpath/go-template against a Secret: a template that errors on a Secret
	// makes kubectl print "raw data was:" followed by the ENTIRE object, values included (measured
	// 2026-09-10, memstore-prod — a `len` on one missing annotation key leaked every Secret in a
	// namespace to a transcript). -o json has no template to fail, and this function is the only
	// code that ever looks at `.data`/`.stringData` — it prints keys and nothing else.
	getArgs := sf.kubectlArgs("get", "secret", sf.secretName, "-n", sf.namespace, "-o", "json")
	out, err := runKubectl(nil, getArgs...)
	if err != nil {
		return emitErr(exitErr, "secrets list: %v", err)
	}
	var sec secretJSON
	if jerr := json.Unmarshal([]byte(out), &sec); jerr != nil {
		return emitErr(exitErr, "secrets list: parse kubectl's JSON: %v", jerr)
	}
	keySet := map[string]bool{}
	for k := range sec.Data {
		keySet[k] = true
	}
	for k := range sec.StringData {
		keySet[k] = true
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	emit(map[string]any{"secret_name": sf.secretName, "namespace": sf.namespace, "keys": keys})
	return exitOK
}

// lastAppliedAnnotation is the ONE annotation `apply` maintains and this command refuses to carry
// forward — see the package doc and docs/DEPLOY-ARGUS.md §4.3 ("the single most consequential line
// on this page"). `kubectl get -o json` echoes it back if a PAST `apply` ever wrote it (a second,
// unencrypted copy of every value in this Secret); `secrets migrate` drops it from whatever it
// writes back, same as `secrets set` never creates it in the first place (patch, not apply).
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// dropLastAppliedAnnotation removes lastAppliedAnnotation from a decoded Secret object's
// metadata.annotations in place (a no-op if it was never there), and removes the now-empty
// annotations map too so the replacement object is byte-for-byte what a Secret with no annotations
// looks like, not an object carrying an empty `{}`.
func dropLastAppliedAnnotation(obj map[string]any) {
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		return
	}
	anns, _ := meta["annotations"].(map[string]any)
	if anns == nil {
		return
	}
	delete(anns, lastAppliedAnnotation)
	if len(anns) == 0 {
		delete(meta, "annotations")
	}
}

// cmdSecretsMigrate renames migrateFromKey to migrateToKey on the instance's exec-tokens Secret,
// in place, server-side (msgbus tester follow-up item 3, 2026-09-28). See secretsUsage for the
// full contract; the short version:
//
//   - old absent, new absent  -> clear error, nothing written.
//   - old present, new absent -> the real rename: move the value, drop the old key.
//   - old absent, new present -> already migrated (or a fresh >=0.3.40 render that already dropped
//     the old key by hand) — idempotent success, nothing written.
//   - both present, SAME value -> nothing to do; with --drop-old, drop the old key.
//   - both present, DIFFERENT value -> refuse. Overwriting either silently could strand whichever
//     credential a running executor is actually using.
//
// The value is read via `kubectl get -o json` (base64 in `.data`, per the Secret wire format),
// moved as an opaque base64 STRING (never decoded — nothing in this function ever holds the
// plaintext), and written back via `kubectl replace -f -` on stdin. Never `kubectl apply` (see the
// package doc: apply copies `.data` into the last-applied-configuration annotation, a second
// unencrypted copy of every value in this Secret) — replace does not maintain that annotation, and
// this command drops one if a past `apply` already wrote it.
func cmdSecretsMigrate(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "-help", "--help", "help":
			fmt.Print(secretsUsage)
			return exitOK
		}
	}
	fs := flag.NewFlagSet("secrets migrate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var sf secretsCommonFlags
	sf.bind(fs)
	// COPY by default, never MOVE: an executor older than 0.3.40 reads ONLY the old name, so deleting
	// it would leave that executor with no credential after its next restart. With both keys set, a
	// 0.3.40+ executor reads the new one and stays silent (envname.Lookup: the new name wins, no
	// deprecation line), so keeping the old key costs nothing until every executor has moved.
	dropOld := fs.Bool("drop-old", false, "also delete the old "+migrateFromKey+" key — only once the executor runs 0.3.40 or later, which reads "+migrateToKey+"; an older executor reads only the old key and loses its credential on the next restart")
	restart := fs.Bool("restart", false, "also run `kubectl rollout restart deployment/executor -n <namespace>` — envFrom: secretRef only sets a pod's environment at start, so a migrated key has no effect until the next restart")
	if err := fs.Parse(args); err != nil {
		return emitErr(exitUsage, "secrets migrate: flag error: %v\n%s", err, secretsUsage)
	}
	if strings.TrimSpace(sf.namespace) == "" {
		return emitErr(exitUsage, "secrets migrate: --namespace is required\n%s", secretsUsage)
	}

	getArgs := sf.kubectlArgs("get", "secret", sf.secretName, "-n", sf.namespace, "-o", "json")
	raw, err := runKubectl(nil, getArgs...)
	if err != nil {
		return emitErr(exitErr, "secrets migrate: %v", err)
	}
	// A generic map, not secretJSON: `replace -f -` needs the WHOLE object back (metadata,
	// resourceVersion, type, every other key) untouched except for `.data` and the one annotation
	// this command strips — a typed struct would silently drop any field it does not declare.
	var obj map[string]any
	if jerr := json.Unmarshal([]byte(raw), &obj); jerr != nil {
		return emitErr(exitErr, "secrets migrate: parse kubectl's JSON: %v", jerr)
	}
	data, _ := obj["data"].(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	oldVal, hasOld := data[migrateFromKey]
	newVal, hasNew := data[migrateToKey]

	result := map[string]any{
		"secret_name": sf.secretName, // not "secret": transcript secret-guards redact a field by that name
		"namespace":   sf.namespace,
		"from_key":    migrateFromKey,
		"to_key":      migrateToKey,
	}

	switch {
	case !hasOld && !hasNew:
		return emitErr(exitErr, "secrets migrate: neither %s nor %s is set on secret %s/%s — nothing to migrate\n%s",
			migrateFromKey, migrateToKey, sf.namespace, sf.secretName, secretsUsage)
	case !hasOld && hasNew:
		result["migrated"] = false
		result["already_migrated"] = true
		emit(result)
		return exitOK
	case hasOld && hasNew:
		// Both are base64 STRINGS straight from the Secret's wire format (encoding/base64's
		// StdEncoding is canonical — the same bytes always produce the same string), so a string
		// compare answers "same value?" without ever decoding either into plaintext.
		oldStr, _ := oldVal.(string)
		newStr, _ := newVal.(string)
		if oldStr != newStr {
			return emitErr(exitErr, "secrets migrate: %s and %s both exist on secret %s/%s with DIFFERENT values — refusing to overwrite either; resolve by hand\n%s",
				migrateFromKey, migrateToKey, sf.namespace, sf.secretName, secretsUsage)
		}
		if !*dropOld {
			result["migrated"] = false
			result["already_migrated"] = true
			result["old_key_kept"] = true
			result["note"] = "both keys already carry the same value; pass --drop-old to delete the old key once the executor runs 0.3.40 or later"
			emit(result)
			return exitOK
		}
		delete(data, migrateFromKey)
		result["migrated"] = true
		result["note"] = "both keys already carried the same value; dropped the old key (--drop-old)"
	default: // hasOld && !hasNew — the real rename
		data[migrateToKey] = oldVal
		if *dropOld {
			delete(data, migrateFromKey)
		}
		result["migrated"] = true
	}
	result["old_key_kept"] = !*dropOld

	obj["data"] = data
	dropLastAppliedAnnotation(obj)

	payload, merr := json.Marshal(obj)
	if merr != nil {
		return emitErr(exitErr, "secrets migrate: build replacement object: %v", merr)
	}
	replaceArgs := sf.kubectlArgs("replace", "-f", "-")
	if _, rerr := runKubectl(bytes.NewReader(payload), replaceArgs...); rerr != nil {
		return emitErr(exitErr, "secrets migrate: %v", rerr)
	}

	restartCmd := sf.restartCommand()
	result["restart_cmd"] = restartCmd
	result["why_restart"] = "the executor Deployment reads exec-tokens via envFrom: secretRef (internal/k8srender/k8srender.go) — that only sets a pod's environment at start, so a migrated key has no effect on an already-running pod until it restarts"
	result["alias_ends"] = "the " + migrateFromKey + " alias stays for every 0.3.x release and is removed no earlier than v0.4.0"
	if *restart {
		restartArgs := sf.kubectlArgs("rollout", "restart", "deployment/executor", "-n", sf.namespace)
		if _, rerr := runKubectl(nil, restartArgs...); rerr != nil {
			result["restarted"] = false
			result["restart_error"] = rerr.Error()
			emit(result)
			return exitErr
		}
		result["restarted"] = true
	} else {
		result["restarted"] = false
	}
	emit(result)
	return exitOK
}
