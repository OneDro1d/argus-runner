package compare

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// approval.go -- ARGUS-CMP-10: the pure half of an approved difference.
//
// An approval accepts ONE observed difference: the output a member gave, set against the output the reference gave.
// It is therefore pinned to the two hashes of the cell it was given for, and counts only while the cell still shows
// exactly those (see calc.applyApprovals). This file holds the two things that are about the approval itself: which
// STEPS of a chain check a cell differs in (an approval is given per step), and the hash a chain version carries.

// Domains of the approval hash. A revocation has its own, so that no approval's hash can be passed off as a
// revocation's or the other way round, even for the same record at the same instant.
const (
	approvalDomain        = "argus-comparison/approval/1"
	approvalRevokedDomain = "argus-comparison/approval-revoked/1"
)

// ApprovalRecord is what the approval hash is taken over: the identity of the approval and of the cell it is pinned
// to, the two hashes it is pinned to and the instant (the database clock's, passed in: this package reads no clock).
// NOT in it, on purpose: the reason and the approver, which are the author's free text. They never reach a chain,
// not even as a commitment inside a hash (a short reason would be guessable from it).
type ApprovalRecord struct {
	ApprovalID    string
	ComparisonID  string
	Check         string
	Step          string
	Member        string
	VersionKey    string
	ReferenceHash string
	MemberHash    string
	At            time.Time // approved_at for an approval, revoked_at for a revocation
}

// ApprovalHash is the `approval_hash` a comparison's chain version carries (design 8, 7.5). THE RECIPE, for a third
// party who holds the approval's record:
//
//	hash = SHA-256, lowercase hex, over
//	       domain "\n"
//	       then, for each field in this order, <decimal byte length of v> ":" <v> "\n":
//	       approval_id, comparison_id, check (scenario id), step, member, version_key, reference_hash, member_hash,
//	       at (UTC, RFC 3339 with nanoseconds)
//	domain = "argus-comparison/approval/1" for an approval, "argus-comparison/approval-revoked/1" for a revocation
//
// The length prefix makes the encoding unambiguous. A revocation hashes the same record with its own domain and
// revoked_at as `at`, so the two versions of one approval differ and neither can be replayed as the other.
func ApprovalHash(rec ApprovalRecord, revoked bool) string {
	h := sha256.New()
	d := approvalDomain
	if revoked {
		d = approvalRevokedDomain
	}
	fmt.Fprintf(h, "%s\n", d)
	for _, v := range []string{rec.ApprovalID, rec.ComparisonID, rec.Check, rec.Step, rec.Member, rec.VersionKey, rec.ReferenceHash, rec.MemberHash,
		rec.At.UTC().Format(time.RFC3339Nano)} {
		fmt.Fprintf(h, "%d:%s\n", len(v), v)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// stepNames lists the distinct steps of a set of rows, sorted. A check with no named step has the one step "".
func stepNames(recs []ScenarioOutput) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range recs {
		if !seen[r.Step] {
			seen[r.Step] = true
			out = append(out, r.Step)
		}
	}
	sort.Strings(out)
	return out
}

func rowsOfStep(recs []ScenarioOutput, step string) []ScenarioOutput {
	var out []ScenarioOutput
	for _, r := range recs {
		if r.Step == step {
			out = append(out, r)
		}
	}
	return out
}

// differingSteps names the steps in which got differs from ref, by the very comparison the cell was judged with
// (RecordsAgree), step by step. A step present on one side only differs. Sorted.
func differingSteps(rules *Rules, ref, got []ScenarioOutput) []string {
	set := map[string]bool{}
	for _, s := range stepNames(ref) {
		set[s] = true
	}
	for _, s := range stepNames(got) {
		set[s] = true
	}
	var out []string
	for s := range set {
		if ok, _ := RecordsAgree(rules, rowsOfStep(ref, s), rowsOfStep(got, s)); !ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func unionSorted(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// applyApprovals decides which of the stored approvals count for one cell, and lists them all on it.
//
//	An approval counts only when ALL of these hold:
//	  1. the cell's state, judged before any approval, is `differs` (never noise, not_measured, could_not_run or
//	     identical: an approval accepts a difference that was SEEN, and those cells hold none);
//	  2. the approval's reference hash and member hash are exactly the cell's two hashes now. The cell's hashes
//	     cover EVERY usable run of the reference and of the member (see measuredCell), so an approval can never
//	     cover an output that no one looked at: a new output on either side moves a hash and voids it;
//	  3. it is not revoked;
//	  4. its step is one of the steps the cell differs in.
//	A cell reads approved only when EVERY step it differs in has such an approval. A check with no named step has the
//	one step "". A seed cell is never approved: an approval must not hide that two systems did not start alike.
//
// Every approval pinned to this check, member and version is listed with why it does or does not count (live,
// revoked or stale), so the author can see why a cell no longer reads approved. A revoked or stale approval leaves
// the cell exactly as it was judged: `differs`.
func (c *calc) applyApprovals(ck *Check, cell *Cell) {
	var refs []ApprovalRef
	covered := map[string]bool{}
	need := map[string]bool{}
	for _, s := range cell.DiffSteps {
		need[s] = true
	}
	eligible := cell.State == StateDiffers && !ck.Seed && !cell.Reference
	for _, a := range c.in.Approvals {
		if a.Check != ck.ID || a.Member != cell.Member || a.VersionKey != cell.VersionKey {
			continue
		}
		status := ApprovalStale
		switch {
		case a.Revoked:
			status = ApprovalRevoked
		case eligible && a.ReferenceHash == cell.ReferenceHash && a.MemberHash == cell.MemberHash && need[a.Step]:
			status = ApprovalLive
			covered[a.Step] = true
		}
		refs = append(refs, ApprovalRef{ID: a.ID, Step: a.Step, Status: status})
	}
	if len(refs) == 0 {
		return
	}
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].ID != refs[j].ID {
			return refs[i].ID < refs[j].ID
		}
		return refs[i].Step < refs[j].Step
	})
	cell.Approvals = refs
	if !eligible || len(need) == 0 {
		return
	}
	all := true
	for s := range need {
		if !covered[s] {
			all = false
		}
	}
	cell.Approved = all
}
