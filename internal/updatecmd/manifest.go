package updatecmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// V31-001 1-MANIFEST (VR13-UP) — THE INSTALLED MANIFEST.
//
// ⛔ THE DEFECT IT CLOSES. An update refreshed the executor and left everything else on the version
// it was onboarded with — the kit, the skills, the agent config, the observability stack — and
// NOBODY COULD SAY WHICH. There was no record of what is installed, so "is this instance current?"
// had no answer, and the page's only honest option was to say nothing.
//
// ⛔ ONE WRITER. `argus write-manifest` (onboarding's last step) and `argus update commit`
// (after every successful apply) both come through this file. Two writers would drift, and the
// drift would surface as a page that contradicts the machine it describes.
//
// ⛔ IT IS A RE-HASH OF REALITY, NEVER A COPY OF THE PLAN — and most of all on `rollback-failed`,
// the one outcome that exists because the machine is where NEITHER version says it should be.
// Writing the plan there would write a comfortable fiction over the one state that needs the truth.
//
// ⛔ AND NO SECRET REACHES IT. It sits on disk beside the kit, the control plane holds a copy, and
// the Environments page renders from it: three places a token must never be. TestManifest_
// CarriesNoSecret greps the written bytes.

// Artefact is one thing onboarding installed, and what version of it is here.
type Artefact struct {
	// Kind: executor | kit | skills | agentcfg | obs_stack | promtail_cfg | k8s_object …
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Version is "unknown" when the artefact carries no version we can read — which is a fact worth
	// recording rather than a reason to omit it. An unknown version is REFRESHED unconditionally:
	// not knowing is not evidence of being current.
	Version string `json:"version"`
	// Image is set on anything that runs from one, digest-pinned.
	Image string `json:"image,omitempty"`
	// Hash is set only on files WE own — a third party's file is not ours to police.
	Hash string `json:"hash,omitempty"`
	// Runtime discriminates an obs_stack entry: compose_service | k8s_deployment | k8s_daemonset.
	Runtime string `json:"runtime,omitempty"`
	// Namespace is set on the k8s runtimes (SA-D20).
	Namespace string `json:"namespace,omitempty"`
	// Loc records WHERE a file artefact lives, so an operator can find it without guessing.
	Loc *Loc `json:"loc,omitempty"`
	// Shared marks an artefact that is ONE object for the whole machine, not this instance's own — the router
	// (AC-D59, #393). Its version here is a COPY of one reading; another instance's update may have moved it
	// since, so a reader must never present it as this instance's current fact.
	Shared bool `json:"shared,omitempty"`
	// ReadAt is when a Shared artefact's version was read (RFC 3339). "" on a manifest written before AC-D59.
	ReadAt string `json:"read_at,omitempty"`
}

// Loc is an artefact's location on this machine.
type Loc struct {
	HostPath string `json:"host_path,omitempty"`
	PathMode string `json:"path_mode,omitempty"` // native | msys | mixed — how the path is spelled
}

// Previous is where this instance was BEFORE the last forward update.
//
// ⛔ A ROLLBACK DOES NOT TOUCH IT (SA-D10 / P-9). If a rollback moved it, rolling back to 0.3.31
// would record `previous = 0.3.32`, the page would offer a rollback TO the version just left, and an
// operator could ping-pong forever without either state looking wrong.
type Previous struct {
	Version   string `json:"version"`
	Image     string `json:"image,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// UnrecordedPrefix marks an update that CHANGED THE MACHINE and could not write its record (AC-D58, #392):
// `unrecorded-updated` and `unrecorded-rolled-back-to <version>`. It is a prefix, never a suffix, so every
// consumer that matches `updated` or `rolled-back-to ` by prefix or equality correctly does NOT match it.
const UnrecordedPrefix = "unrecorded-"

// OutcomeUnrecorded reports whether the word says the machine changed but its record was not written.
func OutcomeUnrecorded(word string) bool { return strings.HasPrefix(word, UnrecordedPrefix) }

// OutcomeNeedsAttention reports whether a word must be shown as needing attention: everything except a
// success (`updated`, `rolled-back-to <version>`) and `untouched` (nothing moved). An unknown word needs
// attention too — a word nobody here knows is never read as success.
func OutcomeNeedsAttention(word string) bool {
	switch {
	case word == "updated", word == "untouched", strings.HasPrefix(word, "rolled-back-to "):
		return false
	}
	return true
}

// LastOutcome is how the last attempt ended, in the PO's own words:
// untouched · updated · rolled-back · rollback-failed · rolled-back-to <version>, and — AC-D58 — the
// unrecorded forms `unrecorded-updated` · `unrecorded-rolled-back-to <version>` (UnrecordedPrefix)
// — and, since AC-D53, rollback-unreachable: the undo could not run because the container runtime (or the
// Kubernetes API) stopped answering. It is deliberately NOT rollback-failed, so the page block does not refuse
// the next run (updatecmd.go RenderBlock), and the executor it names is recorded unknown (rehash.go).
type LastOutcome struct {
	Word       string `json:"word"`
	At         string `json:"at"`
	FailedStep string `json:"failed_step,omitempty"`
	// Health and Detail are AC-D53's (design step 8): when the move of the executor (A-3) failed its health check,
	// HOW — `unhealthy` (the runtime answered, and that is what it said) or `unreachable` (it could not be asked,
	// which says nothing about the executor) — and the one line of output that decided it. Empty when A-3 did not
	// fail on its health check. Without them the control plane saw `rolled-back` + `A-3` alike for an executor the
	// runtime could not be asked about and one it saw broken. Optional fields: the control plane stores the report
	// body as it arrives (fed.go handleInstalled), so an older one keeps them too.
	Health string `json:"health,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Manifest is what is installed for one instance.
type Manifest struct {
	InstanceID  string       `json:"instance_id"`
	Tier        string       `json:"tier"`
	Version     string       `json:"version"`
	Executor    string       `json:"executor,omitempty"` // the digest-pinned executor image
	Artefacts   []Artefact   `json:"artefacts,omitempty"`
	Previous    *Previous    `json:"previous,omitempty"`
	LastOutcome *LastOutcome `json:"last_outcome,omitempty"`
	WrittenAt   string       `json:"written_at,omitempty"`
	// Unconfirmed is set by ReadManifest when apply.sh's not-confirmed marker stands beside the record.
	// ⛔ NEVER WRITTEN INTO THE RECORD (WriteManifest drops it): it is the machine's own fact about a move in
	// progress, and the marker file is its only store. ⛔ BUT IT IS REPORTED: `update report` posts ReadManifest's
	// view, and the page reads the control plane's copy back through this type — without the field on the wire,
	// RollbackReason's branch for it could never run, and the page said nothing for a version the marker had made
	// unknown (its "" means "no manifest has ever been recorded").
	Unconfirmed *Unconfirmed `json:"unconfirmed,omitempty"`
}

// Unconfirmed is apply.sh's "not confirmed" marker (AC-D53 criterion k): an update began to move this instance's
// executor, FROM one version TO another, and could not confirm where it ended. The executor is then on one of the
// versions the marker names (plan.go unconfirmedVersions), and nobody knows which — until a later reading of the
// executor itself says (CommitManifest).
type Unconfirmed struct {
	Image   string `json:"image,omitempty"`   // the image that update was moving the executor to — what `argus up` re-runs
	Version string `json:"version,omitempty"` // the version it was moving it to
	// RollbackTo is set when that move was a deliberate rollback (the page's rollback block). ⛔ `argus up` has no
	// rollback flag: re-running the marker's image as a FORWARD update would run the plan inside the older image,
	// record `updated` for a rollback, move `previous`, and refresh the older bundle's scenarios (A-6).
	RollbackTo string `json:"rollback_to,omitempty"`
	// FromVersion and FromImage are where the executor was before that move, as its plan read it (markerFrom) —
	// "" when nothing could be read. They are what `previous` becomes once the move is seen to have landed.
	FromVersion string `json:"from_version,omitempty"`
	FromImage   string `json:"from_image,omitempty"`
	// FromPrevious is the `previous` that was TRUE for the executor where that plan found it — what `previous` stays
	// when the move did not land, landed on the version it was already on, or was a rollback (P-9). ⛔ NOT THE RECORD'S
	// previous at commit time: after an earlier unconfirmed move that landed, the record is stale (compose: that
	// run's commit could not run; k3d: it committed the executor unknown and kept the old previous), and its previous
	// is one release too far back. The plan resolves the earlier marker; nil when it could not (nothing is invented).
	FromPrevious *Previous `json:"-"`
	// FromOneOf is set when that plan could NOT read the executor and an earlier marker stood: every version the
	// earlier marker left it on (FromVersion is then ""). The downgrade gate counts them all (plan.go
	// installedForGate). Never reported: it may carry the record's version, which on compose is the stale half.
	FromOneOf []string `json:"-"`
	At        string   `json:"at,omitempty"` // when the move began (UTC, RFC 3339)
	// RecordVersion is the version the record held under the marker. Set by ReadManifest for the plan's gates
	// (installedForGate); never reported: on compose it is the stale half.
	RecordVersion string `json:"-"`
}

// unconfirmedPath is the marker beside the record. Not ".json": the sibling enumeration reads only records.
func unconfirmedPath(routerState, instanceID string) string {
	return filepath.Join(routerState, "installed", instanceID+".unconfirmed")
}

// CommitOpts says how this commit came about.
type CommitOpts struct {
	// RollbackTo is non-empty when this commit is a rollback — the ONE case in which Previous is
	// left alone.
	RollbackTo string
	// Outcome and FailedStep record how the attempt ended. Outcome defaults to "updated".
	Outcome    string
	FailedStep string
	// Now is injectable so a test can assert the stamp; zero means time.Now().
	Now time.Time
}

func manifestPath(routerState, instanceID string) string {
	return filepath.Join(routerState, "installed", instanceID+".json")
}

// WriteManifest writes the manifest for one instance, replacing whatever was there.
//
// ⚠ 0600 in a 0700 directory: it is not secret, but it describes this machine's layout in detail and
// there is no reason for anything else on the box to read it.
func WriteManifest(routerState string, m Manifest) error {
	if m.InstanceID == "" {
		return errors.New("a manifest with no instance id describes nothing")
	}
	if !federation.ValidInstanceID(m.InstanceID) {
		// the id is the file name under the router state: `..` in it would write somewhere else entirely
		return fmt.Errorf("instance id %q is not an instance name onboarding accepts — refusing to build a path from it", m.InstanceID)
	}
	if m.WrittenAt == "" {
		m.WrittenAt = time.Now().UTC().Format(time.RFC3339)
	}
	// the marker is its own file, never part of the record (see Manifest.Unconfirmed)
	m.Unconfirmed = nil
	dir := filepath.Join(routerState, "installed")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath(routerState, m.InstanceID), append(b, '\n'), 0o600)
}

// ReadManifest reads one instance's manifest.
//
// ⚠ AN ABSENT MANIFEST IS NOT AN ERROR. Every instance onboarded before 0.3.32 has none, which is a
// fact about the estate rather than a failure — `update plan` reconstructs one. Callers must be able
// to tell "absent" from "broken", so absent comes back as a zero Manifest and a nil error.
//
// ⛔ AC-D53 criterion (k): WHILE THE NOT-CONFIRMED MARKER STANDS, THE EXECUTOR IS UNKNOWN. apply.sh writes the
// marker before it moves the executor; on compose, when the container runtime stops answering, `update rehash`
// and `update commit` cannot run either (they run in containers), so the record from BEFORE the update stays —
// and read as it is, it names the version before for an executor that may be on the new one: a confident claim
// made of an absence. So every reader goes through here and sees the executor, and the version that follows it,
// as unknown — the same shape the re-hash writes when it can run. The marker alone is not a record: with no
// manifest, the instance still reads as absent. CommitManifest reads the RECORD (readRecord), never this view.
func ReadManifest(routerState, instanceID string) (Manifest, error) {
	m, err := readRecord(routerState, instanceID)
	if err != nil {
		return Manifest{}, err
	}
	u, err := readUnconfirmed(routerState, instanceID)
	if err != nil || u == nil {
		return m, err
	}
	m.Unconfirmed = u
	if m.InstanceID == "" {
		return m, nil
	}
	u.RecordVersion = m.Version
	m.Version, m.Executor = "", ""
	arts := make([]Artefact, len(m.Artefacts))
	copy(arts, m.Artefacts)
	for i := range arts {
		if arts[i].Kind == "executor" {
			arts[i].Version, arts[i].Image = "unknown", ""
		}
	}
	m.Artefacts = arts
	return m, nil
}

// readRecord is the record exactly as written — ReadManifest's view without the not-confirmed marker.
func readRecord(routerState, instanceID string) (Manifest, error) {
	if !federation.ValidInstanceID(instanceID) {
		return Manifest{}, fmt.Errorf("instance id %q is not an instance name onboarding accepts — refusing to build a path from it", instanceID)
	}
	b, err := os.ReadFile(manifestPath(routerState, instanceID))
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, nil
	}
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("the manifest for %s is not readable JSON: %w", instanceID, err)
	}
	return m, nil
}

// readUnconfirmed reads apply.sh's marker: `key=value` lines (image=, version=, rollback_to= on a rollback,
// from_version=, from_image=, from_previous_version=, from_previous_image=, from_previous_at=, from_one_of= when its
// plan could not read the executor under an earlier marker, at=), written by bash with no runtime.
// ⛔ A MARKER THAT EXISTS BUT CANNOT BE PARSED STILL MEANS "NOT CONFIRMED": its presence is the fact.
func readUnconfirmed(routerState, instanceID string) (*Unconfirmed, error) {
	b, err := os.ReadFile(unconfirmedPath(routerState, instanceID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the not-confirmed marker for %s could not be read: %w", instanceID, err)
	}
	u := &Unconfirmed{}
	var fp Previous
	defer func() {
		// no version: no previous stood there, or the plan could not tell which — either way none is kept
		if fp.Version != "" {
			u.FromPrevious = &fp
		}
	}()
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "image":
			u.Image = strings.TrimSpace(v)
		case "version":
			u.Version = strings.TrimSpace(v)
		case "rollback_to":
			u.RollbackTo = strings.TrimSpace(v)
		case "from_version":
			u.FromVersion = strings.TrimSpace(v)
		case "from_image":
			u.FromImage = strings.TrimSpace(v)
		case "from_previous_version":
			fp.Version = strings.TrimSpace(v)
		case "from_previous_image":
			fp.Image = strings.TrimSpace(v)
		case "from_previous_at":
			fp.UpdatedAt = strings.TrimSpace(v)
		case "from_one_of":
			u.FromOneOf = strings.Fields(v)
		case "at":
			u.At = strings.TrimSpace(v)
		}
	}
	return u, nil
}

// CommitManifest writes the manifest after an apply and returns what it wrote.
//
// It carries `previous` forward according to the one rule that matters: a FORWARD update records
// where the machine was; a ROLLBACK leaves that record exactly as it is.
func CommitManifest(routerState string, next Manifest, opts CommitOpts) (Manifest, error) {
	// ⛔ THE RECORD, NOT ReadManifest's VIEW OF IT: every update writes the not-confirmed marker before its move,
	// so through ReadManifest the prior version would read "unknown" at every commit — and `previous`, which is
	// where the instance came from, would never be recorded again.
	prior, err := readRecord(routerState, next.InstanceID)
	if err != nil {
		return Manifest{}, err
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	stamp := now.UTC().Format(time.RFC3339)
	// the marker standing NOW: this run's own when it moved the executor (written before the move), else an earlier
	// run's (a move that never started puts the earlier one back as it was — scripts_apply.go restore_marker)
	u, err := readUnconfirmed(routerState, next.InstanceID)
	if err != nil {
		return Manifest{}, err
	}

	switch {
	case !executorKnown(next.Version):
		// ⛔ AN UNKNOWN EXECUTOR IS NO EVIDENCE OF A MOVE: `previous` stays. (It used to become the version before the
		// move — so when the move then turned out NOT to have landed, the next known commit recorded `previous` equal
		// to the version itself: no rollback offered, and a page saying "right after a rollback" that never happened.)
		next.Previous = prior.Previous
	case u != nil:
		// ⛔ A MOVE NOBODY CONFIRMED, AND THE EXECUTOR IS KNOWN AGAIN: the marker, not the record, says what happened.
		// On compose the record under it is stale (the interrupted attempt's own commit could not run), so reading it
		// named the release the operator had rolled away from; and a rule keyed on a flag applied it twice when a
		// commit was retried. The marker is read as it stands, so a retried commit — the marker gone — records the same.
		// ⛔ BEFORE THE ROLLBACK CASE: a rollback that did not land leaves the executor where it was, and the record's
		// previous is stale exactly when an earlier unconfirmed move had landed — the marker carries the true one.
		next.Previous = previousAfterUnconfirmed(u, next.Version, stamp)
	case opts.RollbackTo != "":
		// ⛔ THE ROLLBACK CASE: previous is CARRIED OVER untouched. See Previous's own note.
		next.Previous = prior.Previous
	case executorKnown(prior.Version) && prior.Version != next.Version && !sameBinary(prior.Executor, next.Executor):
		// ⛔ ONLY ON ANOTHER BINARY (round-6 review BIN-1): on the same one the record's version was a wrong label — the
		// ARGUS_VERSION override a run before round 6 wrote — so previous stays and only the version is corrected
		next.Previous = pinnedPrevious(prior.Version, prior.Executor, stamp)
	case prior.Version == "" && next.Previous != nil:
		// ⭐ THE FIRST MANIFEST OF AN INSTANCE ONBOARDED BEFORE 0.3.32 (V32 Release QA (d)). There is no
		// record to take `previous` from, so the re-hash's reading of the executor that ran before is
		// kept — Rehash sets it only for a real, older, imaged version. Without this no existing
		// instance could ever be offered the rollback block.
		next.Previous.UpdatedAt = stamp
	default:
		// nothing moved (a re-hash, or the first write) — keep whatever was recorded
		next.Previous = prior.Previous
	}

	word := opts.Outcome
	if word == "" {
		word = "updated"
	}
	lo := &LastOutcome{Word: word, At: stamp, FailedStep: opts.FailedStep}
	if next.LastOutcome != nil {
		// design step 8: A-3's health verdict comes from the re-hash (this attempt's progress.json) — the commit
		// restamps the word, time and step, and must not drop it
		lo.Health, lo.Detail = next.LastOutcome.Health, next.LastOutcome.Detail
	}
	next.LastOutcome = lo
	next.WrittenAt = stamp
	if err := WriteManifest(routerState, next); err != nil {
		return Manifest{}, err
	}
	// AC-D53: a record with a known executor ends the not-confirmed state. One the re-hash recorded unknown
	// (Version "", or the literal "unknown" it writes for an executor nobody could read — rehash.go
	// previousVersionFrom) keeps the marker: it still carries the image the move was going to, for the re-run, and the
	// versions the downgrade gate must count.
	// ⚠ A marker that cannot be removed is left, and the record keeps reading "unknown": the safe direction.
	// The next commit of a known executor tries again.
	if executorKnown(next.Version) {
		_ = os.Remove(unconfirmedPath(routerState, next.InstanceID))
	}
	return next, nil
}

// executorKnown reports whether a recorded version is a reading of the executor: not "" (the re-hash's "not known",
// as the control plane stores it) and not the literal "unknown" (what the re-hash writes when nothing was read).
func executorKnown(v string) bool {
	return v != "" && v != "unknown"
}

// sameBinary reports whether two image references name the same binary: only when both are pinned to a digest and the
// digests are equal. A tag names whatever was pushed last — managed tiers render imagePullPolicy Always for a MOVING tag
// such as :m3-dev (internal/k8srender) — and a repository name is only a name, one digest can carry several (round-7
// review R7-1/R7-2). Anything else is not known to be the same binary.
func sameBinary(a, b string) bool {
	_, da, okA := strings.Cut(a, "@sha256:")
	_, db, okB := strings.Cut(b, "@sha256:")
	return okA && okB && da != "" && da == db
}

// previousAfterUnconfirmed is `previous` once the executor is known again, on `version`, after a move the marker names
// (FROM → TO).
//
//   - Anywhere but TO, the move did not land: the executor is where the plan found it, and `previous` is what was
//     true there — the marker's FromPrevious. So is a move to the version it was already on, and a rollback (P-9).
//   - On TO, a forward move landed: `previous` is where it came from — FromVersion/FromImage, what its plan read — stamped
//     with when that move began (the marker's at=; else `stamp`), so the same fact has one time whichever run resolves
//     it. Where it came from unknown: nothing is invented, and no rollback is offered to a guess. ⛔ AND ONLY A PINNED
//     IMAGE (the V32 adversary gate, rehash.go): the plan reads the executor's image as discover saw it — on k3d the
//     Deployment's image string, a TAG when the instance was onboarded from one — and the rollback block can never be
//     rendered for a tag (RenderBlock), so a tag recorded here offered a rollback the page could neither show nor explain.
//
// The plan calls it too (markerFrom), for an EARLIER marker, with the version discover read: what a commit would record
// if it ran now is the `previous` true where the executor stands — this run's FromPrevious.
func previousAfterUnconfirmed(u *Unconfirmed, version, stamp string) *Previous {
	if version != u.Version || u.RollbackTo != "" || u.FromVersion == version {
		return u.FromPrevious
	}
	if u.At != "" {
		stamp = u.At
	}
	return pinnedPrevious(u.FromVersion, u.FromImage, stamp)
}

// pinnedPrevious is `previous` for a version the executor was read on and the image it ran — ONLY a pinned image
// (the V32 adversary-gate rule, rehash.go): the rollback block names previous.image as --image-digest, which
// `update plan` refuses unless it is repo@sha256:…, so an unpinned one offered a rollback the page could neither
// render nor explain. nil instead, and the page says no previous version is recorded.
func pinnedPrevious(version, image, stamp string) *Previous {
	if !executorKnown(version) || !strings.Contains(image, "@sha256:") {
		return nil
	}
	return &Previous{Version: version, Image: image, UpdatedAt: stamp}
}

// RollbackOffered reports whether the Environments page should show the rollback block.
//
// SA-D10: only when a previous version exists AND is BELOW the installed one, by semver. The block's
// purpose is the hour AFTER a successful update, so:
//   - never updated        → nothing to go back to;
//   - just rolled back     → previous == version, and offering it would loop the operator;
//   - previous is NEWER    → the machine went backwards some other way; the block would go forward;
//   - unparseable version  → ⛔ NO BLOCK, and the reason is stated. A comparison we cannot make is
//     not a comparison that passes: rendering a block on a version nobody can order is how an
//     operator ends up pasting a command that does something else.
//
// ⛔ AC-D49: WHETHER previous IS RECORDED AT ALL is a SEPARATE question from whether it orders below
// the installed version — see Rehash. This function is the ONLY place "is it genuinely older" gets to
// decide anything, and what it decides is just the rendering, never whether the fact was kept.
func RollbackOffered(m Manifest) bool {
	if m.Previous == nil || m.Previous.Version == "" || m.Version == "" {
		return false
	}
	return SemverLess(m.Previous.Version, m.Version)
}

// RollbackReason explains, in one sentence naming both versions where both are known, why
// RollbackOffered answered false. "" means there is nothing honest to add beyond the boolean — the
// instance has no manifest at all (m.Version == ""), which is the ordinary state before a first
// update and not a promise being withheld.
//
// AC-D49 (§296): HostRollbackCommand used to return "" with no placeholder and no reason — the
// renderer's own doc comment promised "no block, and the reason is stated" and then did not state
// one. This is that reason, read by both the page (rollback_block_reason) and `argus update commit`
// (rollback_offered_reason), so the two surfaces cannot say something different.
func RollbackReason(m Manifest) string {
	if m.Version == "" {
		// ⛔ AC-D53: AN UNKNOWN VERSION AFTER AN UPDATE IS NOT "NEVER UPDATED". When the last update left the
		// executor unconfirmed (the re-hash, or the not-confirmed marker, records it unknown), the page must say
		// why no rollback is offered instead of rendering the instance like one that was never updated. Only
		// the facts: what to do next differs (a re-run after rollback-unreachable, a refusal after
		// rollback-failed) and the page block says that itself.
		if m.LastOutcome != nil && (m.LastOutcome.Word == "rollback-unreachable" || m.LastOutcome.Word == "rollback-failed") {
			return "the installed version is unknown after the last update (" + m.LastOutcome.Word + "): its " +
				"executor could not be confirmed, so no rollback is offered"
		}
		if m.Unconfirmed != nil {
			// what the marker says in every case it stands — also after an update that confirmed the executor but
			// could not write its record
			return "the installed version is unknown: an update began to move its executor, and no record of where " +
				"that move ended has been written yet, so no rollback is offered"
		}
		return ""
	}
	// ⛔ SELF-CONSISTENT WITH RollbackOffered, NOT A SEPARATE COPY OF ITS LOGIC. Every branch below
	// composes a sentence for a SPECIFIC way an offer gets withheld; without this guard first, a pair
	// that genuinely IS below (RollbackOffered == true) would still fall through to "is not below" —
	// two functions answering the same question differently. Found by TestUpdateCommit_
	// OffersRollbackFromADevSuffixedPrevious (AC-D49): the first cut of this function said "not below"
	// for 0.3.37-dev+9816842 → 0.3.40, a pair that IS below and IS offered.
	if RollbackOffered(m) {
		return ""
	}
	if m.Previous == nil || m.Previous.Version == "" {
		return fmt.Sprintf("no previous version is recorded for this instance yet (currently on %s)", m.Version)
	}
	pv, iv := m.Previous.Version, m.Version
	if !ValidVersion(pv) || !ValidVersion(iv) {
		return fmt.Sprintf("the previous version (%s) and the installed version (%s) cannot both be "+
			"ordered, so no rollback is offered", pv, iv)
	}
	if pv == iv {
		return fmt.Sprintf("the previous version (%s) equals the installed version (%s) — this is the "+
			"state right after a rollback, and offering another would let an operator ping-pong forever", pv, iv)
	}
	return fmt.Sprintf("the previous version (%s) is not below the installed version (%s) — rolling "+
		"back would move the instance forward, not back", pv, iv)
}

// ValidVersion reports whether v is a version SemverLess can order. Every gate that compares versions
// passes silently on one it cannot, so a caller that is about to rely on a comparison checks this first.
func ValidVersion(v string) bool {
	_, _, ok := parseSemver(v)
	return ok
}

// SemverLess reports whether a is below b. EXPORTED because the control plane ranks the same
// versions for the held-back rule, and a second comparator in that package would be a second answer
// to one question — the drift instanceview.go's own header was written about.
//
// It answers FALSE when either side cannot be parsed at all — see RollbackOffered and ValidVersion:
// an unrankable comparison is not a comparison that passes.
//
// ⛔ AC-D49 PRECEDENCE RULE, STATED ONCE, HERE: the MAJOR.MINOR.PATCH core orders first. When two
// versions share a core, a version carrying a "-"/"+" suffix (a dev build, "0.3.37-dev+9816842") is
// BELOW the bare release of the same core — semver's own rule for a pre-release against its release
// (semver.org §11.4: "a pre-release version has lower precedence than the associated normal
// version"). Two suffixed versions that share a core compare EQUAL: this codebase's suffix is a
// build stamp (`-dev+<sha>`), never a further-ordered pre-release train ("1.0.0-alpha.1" <
// "1.0.0-alpha.2"), so there is no second axis to rank two dev builds of the same core against each
// other, and inventing one would be a comparison this code has no evidence for.
func SemverLess(a, b string) bool {
	pa, aSuffixed, oka := parseSemver(a)
	pb, bSuffixed, okb := parseSemver(b)
	if !oka || !okb {
		return false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	if aSuffixed != bSuffixed {
		return aSuffixed // a carries the suffix, b does not → a is the pre-release, a < b
	}
	return false
}

// parseSemver reads a MAJOR.MINOR.PATCH core, tolerating a leading "v" and a trailing
// "-prerelease+build" suffix — dev builds carry one ("0.3.37-dev+9816842"). suffixed reports whether
// one was cut off, which SemverLess needs for the precedence rule above; ok is false only when the
// numeric core itself cannot be read.
func parseSemver(v string) (core [3]int, suffixed bool, ok bool) {
	s := strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
		suffixed = true
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return core, suffixed, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return core, suffixed, false
		}
		core[i] = n
	}
	// ⛔ AC-D49's suffix-stripping made "0.0.0-dev" and "0.0.0-src+<rev>" PARSEABLE (a real numeric
	// core, "0.0.0", plus a suffix) — but those two are buildinfo's own NO-RELEASE-IDENTITY sentinels
	// for an unstamped build, not a version (control.isNoReleaseIdentity is the same rule for the
	// control plane's own comparator; cmd/argus/update_phase1.go:116 refuses a plan on exactly this).
	// The 0.0.0 is an accident of the sentinel's spelling, not a statement about age: ranking it would
	// call a developer's locally-built executor — typically NEWER than everything in the fleet —
	// "version 0.0.0" and silently switch off the downgrade gate, the shared-folder hold-back and the
	// rollback offer. A BARE "0.0.0" (no suffix — someone actually typed --version 0.0.0) is left
	// alone: only the SENTINEL spelling (suffixed AND all-zero) is refused.
	if suffixed && core == [3]int{0, 0, 0} {
		return core, suffixed, false
	}
	return core, suffixed, true
}
