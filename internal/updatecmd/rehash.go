package updatecmd

import (
	"encoding/json"
	"strings"
)

// rehash.go — V31-001 (VR13-UP) C-15: THE MANIFEST RECORDS WHAT HAPPENED.
//
// ⛔ THE PLAN IS NOT THE ANSWER, AND THAT IS THE WHOLE POINT OF THIS FILE.
//
// The plan describes where the machine was GOING. On a skip it did not go there; on a rollback it
// went and came back; on `rollback-failed` it is somewhere NEITHER version describes. Writing the
// plan as the manifest would lay a comfortable fiction over the one outcome that most needs the
// truth — and it would clear, on the page, exactly the signals this release exists to raise.
//
// So every artefact is resolved against what `progress.json` says actually happened to the step that
// owns it. `applied` → the target version. Anything else — `skipped`, `undone`, `failed`, or NO
// RECORD AT ALL — → the version the machine had before. Absence is not success, here least of all:
// the trap fires mid-run, so the steps after a failure have no record because they never ran.
//
// ⛔ AND ONE STATE IS NEITHER: `unconfirmed` (AC-D53). A step that began to move its artefact records it
// BEFORE the move (scripts_steps.go move_executor); a success overwrites it with `applied`, a clean undo with
// `undone`. When it is still the last word, the move started and NEITHER its health check NOR its undo could be
// completed — the runtime stopped answering. The artefact is then UNKNOWN: recording the version it had
// before would turn an absence into a confident claim, the old version for an executor that may well be
// running the new one. Unknown is what the page already prints for a version nobody could read
// (instanceview.go installedArtefacts), and what the held-back rule already treats as "not known current".

// stepOwners maps an A-step to the artefact kinds it moves. One place, so the script, the plan and
// this resolver cannot disagree about who owns what.
var stepOwners = map[string][]string{
	"A-1":  {"kit"},
	"A-2":  {"k8s_object"},
	"A-3":  {"executor"},
	"A-3a": {"obs_stack"},
	"A-4":  {"promtail_cfg"},
	"A-5":  {"skills"},
	"A-5a": {"agentcfg"},
	"A-6":  {"scenarios"},
	"A-7":  {"router"},
}

// Rehash builds the manifest `update commit` will write.
//
// progress maps a step id to its ACTUAL end state, as apply.sh recorded it.
//
// ⛔ AN ARTEFACT IS AT THE TARGET VERSION ONLY IF A STEP THAT OWNS IT IS RECORDED `applied`. Every
// other reading — skipped, undone, failed, no record, or no owning step in this plan at all — means
// nothing moved it, so it is recorded where the machine really had it. The rule is deliberately
// one-directional: the only evidence that accepts is a positive record of success, because every
// other shape of evidence here is an absence, and an absence has never been success in this system.
func Rehash(p Plan, progress map[string]string, obs Observed, outcome, failedStep string) Manifest {
	moved, unknown := map[string]bool{}, map[string]bool{}
	for _, s := range p.Steps {
		if s.SkipReason != "" {
			continue
		}
		switch progress[s.ID] {
		case "applied":
			for _, kind := range stepOwners[s.ID] {
				moved[kind] = true
			}
		case "unconfirmed":
			for _, kind := range stepOwners[s.ID] {
				unknown[kind] = true
			}
		}
	}

	m := Manifest{
		InstanceID: p.InstanceID,
		Tier:       p.Tier,
		Version:    p.Version,
		Executor:   p.ImageDigest,
		LastOutcome: &LastOutcome{
			Word:       outcome,
			At:         p.GeneratedAt,
			FailedStep: failedStep,
		},
	}

	// ⛔ THE HEADER VERSION FOLLOWS THE EXECUTOR, because that is what the control plane's floors
	// judge and what the page's version verdict is about. An instance whose executor came back is on
	// the old version however many other artefacts moved.
	switch {
	case unknown["executor"]:
		// the header follows the executor, so it is unknown too: "" is how the control plane stores "not known"
		// (store/installed.go NULLIF) and how the held-back rule reads it
		m.Version, m.Executor = "", ""
	case !moved["executor"]:
		m.Version = previousVersionFrom(obs, p)
		m.Executor = observedExecutorImage(obs)
	}

	// ⭐ V32 Release QA (d): A FORWARD UPDATE RECORDS WHERE IT CAME FROM, from the reading taken before
	// anything moved. CommitManifest prefers a recorded manifest; this is the witness for the instance
	// that has none — every instance onboarded before 0.3.32. ⛔ NEVER INVENTED: only a version that
	// was actually read, with the image it ran, becomes `previous`. A rollback records none of its
	// own (P-9 — CommitManifest leaves previous alone).
	//
	// ⛔ AND ONLY A PINNED IMAGE (V32 adversary gate). On k3d/managed discover.sh reads the Deployment's image
	// string, which is a TAG when the executor was onboarded from one (`:slim`). The rollback block names
	// previous.image as --image-digest, which `update plan` refuses unless it is repo@sha256:… — so a tag
	// recorded here offers a rollback that fails every time. No rollback is honest; a broken one is not.
	//
	// ⛔ AC-D49 (#296): RECORDING IS NOT "IS IT OLDER" — that used to be the SAME test (SemverLess), so a
	// version SemverLess could not order (a dev build, "0.3.37-dev+9816842") was silently DISCARDED
	// rather than written down: `before` was read, the old image was digest-pinned, and the only failing
	// term was an ordering test that has nothing to do with whether the fact is worth keeping. The
	// question "should we write it down" is now just "did the executor move, and do we have a real
	// reading of what it moved FROM" — `before` non-empty and different from the target it is moving TO.
	// Whether that reading orders BELOW the target is RollbackOffered's question alone, asked at render
	// time from what was recorded here — never this function's to pre-decide.
	if moved["executor"] && p.RollbackTo == "" {
		before, img := strings.TrimSpace(obs.ExecutorVersion), observedExecutorImage(obs)
		if before != "" && before != p.Version && strings.Contains(img, "@sha256:") {
			m.Previous = &Previous{Version: before, Image: img, UpdatedAt: p.GeneratedAt}
		}
	}

	a7Attempted := routerMoveAttempted(p, progress, failedStep)

	for _, a := range p.Artefacts {
		out := a
		if a.Kind == "router" {
			// ⛔ AC-D59 (#393): ONE ROUTER PER MACHINE, NOT THIS INSTANCE'S. Marked so every reader can say so,
			// with the time of the reading (the plan's time is the nearest witness this function has).
			out.Shared, out.ReadAt = true, p.GeneratedAt
		}
		if unknown[a.Kind] {
			// AC-D53: neither the target nor the previous version: "unknown" below, and no image either
			out.Version, out.Image = "", ""
		} else if a.Kind == "router" && a7Attempted && !moved["router"] {
			// ⛔ AC-D59 (#393), MECHANISM 1: "A-7 did not end applied, so the router is where it was" IS AN
			// ABSENCE OF EVIDENCE TURNED INTO A POSITIVE CLAIM. A-7 was ATTEMPTED — the router may have moved
			// half-way, and another instance's update may have moved it too. Neither the pre-update image nor
			// its version is a fact about the router now: unknown, printed, never the old reading.
			out.Image, out.Version = "", "unknown"
		} else if !moved[a.Kind] {
			// It did not move. Record what the machine really has — which for our own artefacts is
			// the version it was on before, and for a third party is whatever we could read.
			if a.Kind == "executor" {
				out.Version = previousVersionFrom(obs, p)
				out.Image = observedExecutorImage(obs)
			} else if a.Kind == "router" {
				// ⛔ A ROUTER A-7 DID NOT MOVE IS THE ROUTER THE MACHINE RUNS (V32 Release QA defect Q1). The plan's
				// router entry carries the TARGET image, so every rollback — A-7 never moves the machine router
				// back — and every forward update that kept a newer router recorded an image this router never
				// ran. What discover.sh read before anything moved is the record; nothing read is "" / "unknown".
				out.Image = strings.TrimSpace(obs.Router.Image)
				out.Version = strings.TrimSpace(obs.Router.Version)
			} else {
				// ⛔ NOT THE EXECUTOR'S VERSION (V32 adversary gate). What the executor reports says nothing
				// about the kit, the skills or the agent config beside it: update.sh moved the executor
				// alone for releases, and measured on the owner's machine the kit and skills were OLDER
				// than it. A version nobody read is written as "unknown", which is what the page needs
				// in order to show the mixed state it exists to expose.
				out.Version = strings.TrimSpace(obs.Env["ARGUS_INSTALLED_VERSION"])
			}
		}
		if out.Version == "" {
			// ⛔ "unknown" IS PRINTED, NEVER LEFT BLANK. A blank version reads as "no artefact" to
			// every consumer; "unknown" reads as "we looked and could not tell", which is the fact.
			out.Version = "unknown"
		}
		m.Artefacts = append(m.Artefacts, out)
	}
	return m
}

// routerMoveAttempted reports whether A-7 (the only step that moves the machine router) was run against the
// router and did NOT end applied: recorded `undone` or `failed`, or named as the failing step — the last
// covers an undo that itself failed, which leaves no record at all. A-7 absent from the plan, skipped, or
// never reached (an earlier step failed) is NOT an attempt: the router was not touched by this run.
func routerMoveAttempted(p Plan, progress map[string]string, failedStep string) bool {
	for _, s := range p.Steps {
		if s.ID != "A-7" || s.SkipReason != "" {
			continue
		}
		switch progress["A-7"] {
		case "undone", "failed":
			return true
		}
		return failedStep == "A-7" && progress["A-7"] != "applied"
	}
	return false
}

// previousVersionFrom is the version the machine was on before this attempt.
//
// ⚠ IT DEGRADES TO "unknown", NOT TO THE TARGET. An instance reconstructed from observed.json may
// genuinely have no recorded version, and saying so is the honest answer — claiming the target would
// report an update that did not happen.
//
// ⛔ ALSO ON A ROLLBACK (AC-D53). It fell back to the rollback's target: for an executor this attempt neither
// moved nor read, that is the target claimed for a move that did not happen — and since the not-confirmed marker, a
// commit takes any version but "unknown" for a reading (manifest.go executorKnown), removed an earlier run's marker
// and let the downgrade gate open below the version the executor may be on.
func previousVersionFrom(obs Observed, p Plan) string {
	// what the running executor itself said, before anything moved — the direct measurement
	if v := strings.TrimSpace(obs.ExecutorVersion); v != "" {
		return v
	}
	if v := strings.TrimSpace(obs.Env["ARGUS_INSTALLED_VERSION"]); v != "" {
		return v
	}
	return "unknown"
}

// observedExecutorImage is the executor image the host actually saw running.
//
// ⛔ "executor" IS THE NAME: the compose SERVICE (docker-compose.byo-m3.yml:15) and the k8s Deployment
// (update.sh:373). It used to match `argus-executor`, which no onboarding has ever produced — so on
// every real machine this returned "" and a non-moved executor was recorded with no image at all.
func observedExecutorImage(obs Observed) string {
	for _, s := range obs.Compose.Services {
		if s.Name == "executor" {
			if s.Digest != "" {
				return s.Digest
			}
			return s.Image
		}
	}
	for _, o := range obs.K8s.Objects {
		if o.Name == "executor" && o.Image != "" {
			return o.Image
		}
	}
	return ""
}

// marshalManifest is the one serializer, so a test can grep the exact bytes that get written.
func marshalManifest(m Manifest) ([]byte, error) { return json.MarshalIndent(m, "", "  ") }

func containsStr(hay, needle string) bool { return strings.Contains(hay, needle) }
