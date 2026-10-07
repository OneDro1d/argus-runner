package compare

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Result is the one pure function that turns recorded runs into a table and a roll-up verdict
// (design 7). No clock, no store, no network: the same Input always gives the same Output, whatever
// order the runs arrive in. A missing record never becomes "identical": there is no path from
// absent to the same.
func Result(in Input) Output {
	c := newCalc(in)
	return c.run()
}

// JSON is the stable encoding of the output. Hash is taken over exactly these bytes.
func (o Output) JSON() []byte {
	b, _ := json.Marshal(o)
	return b
}

// Hash is the result_hash the ledger anchors: sha256 of the canonical JSON of the whole Output.
func (o Output) Hash() string {
	h := sha256.Sum256(o.JSON())
	return hex.EncodeToString(h[:])
}

// ── classification ──────────────────────────────────────────────────────────────────────────────

type clsKind int

const (
	clsPending clsKind = iota // not terminal yet
	clsCNR                    // could not run
	clsUnrec                  // ran, but nothing usable was recorded for this check
	clsOK
)

const (
	rsnRun     = "run"
	rsnSet     = "set"
	rsnSeed    = "seed"
	rsnErrored = "errored"
)

type cls struct {
	run     *Run
	kind    clsKind
	reason  string
	outcome string
	recs    []ScenarioOutput
}

type calc struct {
	in        Input
	runs      []Run
	members   []Member
	refMember string
	seeds     []string
	refVer    string
	refAmbig  bool
	refUsable bool                // some measured check had at least one usable reference run
	refSingle bool                // some measured check's reference has exactly ONE usable run: its variation is untested
	versions  map[string][]string // member -> sorted version keys
	notes     map[string]bool
}

func newCalc(in Input) *calc {
	c := &calc{in: in, notes: map[string]bool{}, versions: map[string][]string{}}
	// Checks are ordered by id, then path, so the output (and its hash) does not depend on the order the
	// caller listed them in.
	c.in.Checks = append([]Check(nil), in.Checks...)
	sort.SliceStable(c.in.Checks, func(i, j int) bool {
		if c.in.Checks[i].ID != c.in.Checks[j].ID {
			return c.in.Checks[i].ID < c.in.Checks[j].ID
		}
		return c.in.Checks[i].Path < c.in.Checks[j].Path
	})
	c.runs = append([]Run(nil), in.Runs...)
	sort.SliceStable(c.runs, func(i, j int) bool { return c.runs[i].ID < c.runs[j].ID })
	c.members = append([]Member(nil), in.Members...)
	sort.SliceStable(c.members, func(i, j int) bool {
		ri, rj := c.members[i].Role == RoleReference, c.members[j].Role == RoleReference
		if ri != rj {
			return ri
		}
		return c.members[i].Name < c.members[j].Name
	})
	for _, m := range c.members {
		if m.Role == RoleReference {
			c.refMember = m.Name
			break
		}
	}
	for _, ck := range c.in.Checks {
		if ck.Seed {
			c.seeds = append(c.seeds, ck.ID)
		}
	}
	vs := map[string]map[string]bool{}
	for i := range c.runs {
		r := &c.runs[i]
		if vs[r.Member] == nil {
			vs[r.Member] = map[string]bool{}
		}
		vs[r.Member][r.VersionKey] = true
	}
	for m, set := range vs {
		for v := range set {
			c.versions[m] = append(c.versions[m], v)
		}
		sort.Strings(c.versions[m])
	}
	c.refVer = in.ReferenceVersion
	if c.refVer == "" && c.refMember != "" {
		seen := map[string]bool{}
		var terminal []string
		for i := range c.runs {
			r := &c.runs[i]
			if r.Member == c.refMember && runPhase(r) != phasePending && !seen[r.VersionKey] {
				seen[r.VersionKey] = true
				terminal = append(terminal, r.VersionKey)
			}
		}
		switch {
		case len(terminal) == 1:
			c.refVer = terminal[0]
		case len(terminal) > 1:
			c.refAmbig = true
		}
	}
	return c
}

type phase int

const (
	phasePending phase = iota
	phaseRan
	phaseFailed
)

// runPhase says whether a run RAN its checks (ARGUS-CMP-14). The executor pushes status `failed`
// for a run in which some check failed, WITH one outcome per check (runner/mapper.go); that run ran, and its
// verdict is judged like any other. `failed` with no outcome at all is an abort (or a run of zero checks): it never
// ran them. `abandoned` never did. The store holds the same rule (store.ComparisonRun.Class) and a test holds the
// two together.
func runPhase(r *Run) phase {
	switch r.Status {
	case "completed", "degraded":
		return phaseRan
	case "failed":
		if len(r.Outcomes) > 0 {
			return phaseRan
		}
		return phaseFailed
	case "abandoned":
		return phaseFailed
	}
	return phasePending
}

func passedOutcome(o string) bool { return o == OutcomePassed || o == OutcomeDegraded }

func (c *calc) seedFailed(r *Run) bool {
	for _, id := range c.seeds {
		if !passedOutcome(r.Outcomes[id]) {
			return true
		}
	}
	return false
}

func (c *calc) classify(r *Run, ck *Check) cls {
	out := cls{run: r}
	switch runPhase(r) {
	case phasePending:
		out.kind = clsPending
		return out
	case phaseFailed:
		out.kind, out.reason = clsCNR, rsnRun
		return out
	}
	switch {
	case r.SetHash != c.in.SetHash:
		out.kind, out.reason = clsCNR, rsnSet
		return out
	case !ck.Seed && c.seedFailed(r):
		out.kind, out.reason = clsCNR, rsnSeed
		return out
	}
	o, has := r.Outcomes[ck.ID]
	switch {
	case !has:
		out.kind = clsUnrec
		return out
	case o != OutcomePassed && o != OutcomeFailed && o != OutcomeDegraded:
		out.kind, out.reason = clsCNR, rsnErrored
		return out
	}
	out.outcome = o
	if ck.Rules.Reference == RefMeasured {
		for _, row := range r.Outputs {
			// a load record carries numbers and no output (ARGUS-CMP-11): it is never one of the rows an output is
			// compared by, so a check that recorded only that has nothing recorded to compare (below)
			if row.ScenarioID == ck.ID && !isLoadOnly(row.OutputRecord) {
				out.recs = append(out.recs, row)
			}
		}
		if len(out.recs) == 0 {
			out.kind = clsUnrec
			return out
		}
		for _, row := range out.recs {
			if row.State != StateRecorded {
				out.kind = clsUnrec
				return out
			}
		}
	}
	out.kind = clsOK
	return out
}

// ── the run ─────────────────────────────────────────────────────────────────────────────────────

type tally struct {
	judged   int
	unsup    bool // an identical cell whose claim n cannot support
	short    bool
	cnrParts bool
}

func (c *calc) run() Output {
	o := Output{Systems: []System{}, Checks: []CheckResult{}, Performance: []PerfResult{}, Notes: []string{}}
	var counts Counts
	var t tally
	anyUnapproved, anyMeasured, seedBroke, seedGap := false, false, false, false

	for i := range c.in.Checks {
		ck := &c.in.Checks[i]
		if ck.Rules == nil {
			continue
		}
		counts.Checks++
		res, perf := c.evalCheck(ck)
		if ck.Rules.Reference == RefMeasured {
			anyMeasured = true
		}
		for j := range res.Cells {
			cell := &res.Cells[j]
			c.applyApprovals(ck, cell) // ARGUS-CMP-10: which stored approvals count for this cell (approval.go)
			if ck.Seed {
				// a seed cell is shown, never counted. But a seed comparison that does not hold is the one fact it
				// exists to show: the systems did not start alike. A measured seed's reference cell is the baseline.
				// A seed cell that COULD NOT RUN, or was NOT MEASURED (the member has no run yet), judged nothing: that is a
				// gap, not a seed comparison that did not hold (ARGUS-CMP-14), so it keeps the roll-up from `same`
				// without the note that says the systems started differently.
				if !cell.Reference && cell.State != StateIdentical {
					if cell.State == StateCouldNotRun || cell.State == StateNotMeasured {
						seedGap = true
					} else {
						seedBroke = true
					}
				}
				continue
			}
			if cell.Reference {
				// counts.could_not_run counts EVERY non-seed cell in the state could_not_run: a candidate's,
				// and the reference member's own (a reference run of another set, or one that failed, is a gap
				// the table must not hide). A reference cell in any other state is the baseline and is not a
				// judged cell, so no other counter takes it.
				if cell.State == StateCouldNotRun {
					counts.CouldNotRun++
				} else if cell.CouldNotRun > 0 {
					// a reference or control run that could not run is excluded from the control, and the control
					// is then thinner than planned: a gap, never green
					t.short = true
				}
				continue
			}
			t.judged++
			switch cell.State {
			case StateIdentical:
				counts.Identical++
				if !cell.ClaimSupported {
					t.unsup = true
				}
			case StateDiffers:
				if cell.Approved {
					counts.Approved++
				} else {
					counts.Differs++
					anyUnapproved = true
				}
			case StateNoise:
				counts.Noise++
			case StateNotMeasured:
				counts.NotMeasured++
			case StateCouldNotRun:
				counts.CouldNotRun++
			}
			if (cell.State == StateIdentical || cell.State == StateDiffers) && (cell.Short || cell.CouldNotRun > 0) {
				t.short = true
			}
		}
		o.Checks = append(o.Checks, res)
		o.Performance = append(o.Performance, perf...)
	}

	perfWorse, perfGap := 0, false
	for _, p := range o.Performance {
		for _, pc := range p.Cells {
			switch pc.State {
			case BandWorse:
				perfWorse++
			case BandNotMeasured:
				perfGap = true
			}
		}
	}
	counts.Worse = perfWorse
	counts.Members = len(c.in.Members)
	// counts.runs is the TERMINAL runs the table was built from; counts.runs_pending the ones that have not
	// finished (queued, picked up, running). A request that has not run is not a run yet.
	for i := range c.runs {
		if runPhase(&c.runs[i]) == phasePending {
			counts.RunsPending++
		} else {
			counts.Runs++
		}
	}
	o.Counts = counts

	if len(c.seeds) == 0 {
		c.notes[NoteNoSeedStep] = true
	}
	if anyMeasured && c.refMember == "" {
		c.notes[NoteNoReferenceMember] = true
	}
	if anyMeasured && c.refAmbig {
		c.notes[NoteReferenceVersionAmbiguous] = true
	}
	if anyMeasured && !c.refUsable && c.refMember != "" && !c.refAmbig {
		if n := c.noUsableReferenceNote(); n != "" {
			c.notes[n] = true
		}
	}
	if seedBroke {
		c.notes[NoteSeedDidNotHold] = true
	}
	for _, n := range []string{NoteNoSeedStep, NoteNoReferenceMember, NoteReferenceVersionAmbiguous, NoteReferenceSingleRun,
		NoteReferenceOtherSet, NoteReferenceFailed, NoteReferenceNotRun, NoteSeedDidNotHold} {
		if c.notes[n] {
			o.Notes = append(o.Notes, n)
		}
	}
	if n := RunsPendingNote(counts.RunsPending); n != "" {
		o.Notes = append(o.Notes, n)
	}

	o.Systems = c.systems()

	switch {
	case anyUnapproved || perfWorse > 0:
		o.Verdict = VerdictDifferences
	case t.judged == 0 || counts.Noise+counts.NotMeasured+counts.CouldNotRun > 0 || t.unsup || t.short || perfGap ||
		counts.RunsPending > 0 || c.refSingle || seedBroke || seedGap:
		o.Verdict = VerdictIncomplete
	case counts.Approved > 0:
		o.Verdict = VerdictSameWithApproved
	default:
		o.Verdict = VerdictSame
	}
	return o
}

// noUsableReferenceNote says WHY no reference output exists, from the reference member's own runs: it ran a
// different set, it failed before running, or it has not run yet. "" when it ran the right set and simply
// recorded nothing for the checks (each cell then says so itself).
func (c *calc) noUsableReferenceNote() string {
	ran, failed, otherSet := false, false, false
	for i := range c.runs {
		r := &c.runs[i]
		if r.Member != c.refMember {
			continue
		}
		switch runPhase(r) {
		case phaseRan:
			ran = true
			if r.SetHash != c.in.SetHash {
				otherSet = true
			}
		case phaseFailed:
			failed = true
		}
	}
	switch {
	case otherSet:
		return NoteReferenceOtherSet
	case failed && !ran:
		return NoteReferenceFailed
	case !ran && !failed:
		return NoteReferenceNotRun
	}
	return ""
}

func (c *calc) systems() []System {
	out := []System{}
	role := map[string]string{}
	for _, m := range c.members {
		role[m.Name] = m.Role
	}
	for _, m := range c.members {
		vs := c.versions[m.Name]
		if len(vs) == 0 {
			out = append(out, System{Member: m.Name, Role: m.Role, VersionKey: ""})
			continue
		}
		for _, v := range vs {
			n := 0
			for i := range c.runs {
				r := &c.runs[i]
				if r.Member == m.Name && r.VersionKey == v && runPhase(r) == phaseRan {
					n++
				}
			}
			out = append(out, System{Member: m.Name, Role: m.Role, VersionKey: v, Runs: n})
		}
	}
	return out
}

// groupsOf lists (member, version) groups in table order. A member with no run at all still gets one
// (empty) group, so the table shows it as not measured instead of omitting it.
type group struct {
	member, version string
	runs            []cls
}

func (c *calc) groups(ck *Check) []group {
	var out []group
	for _, m := range c.members {
		vs := c.versions[m.Name]
		if len(vs) == 0 {
			out = append(out, group{member: m.Name})
			continue
		}
		for _, v := range vs {
			g := group{member: m.Name, version: v}
			for i := range c.runs {
				r := &c.runs[i]
				if r.Member == m.Name && r.VersionKey == v {
					g.runs = append(g.runs, c.classify(r, ck))
				}
			}
			out = append(out, g)
		}
	}
	return out
}

type groupStats struct {
	ok         []cls
	cnr, unrec int
	pending    int
	reasons    map[string]int
}

func statsOf(g group) groupStats {
	s := groupStats{reasons: map[string]int{}}
	for _, k := range g.runs {
		switch k.kind {
		case clsOK:
			s.ok = append(s.ok, k)
		case clsCNR:
			s.cnr++
			s.reasons[k.reason]++
		case clsUnrec:
			s.unrec++
		case clsPending:
			s.pending++
		}
	}
	return s
}

func claimOf(r *Rules) string {
	switch r.Reference {
	case RefFixed:
		return ClaimFixed
	case RefProperty:
		return ClaimProperty
	}
	return ClaimSameOutput
}

func (c *calc) evalCheck(ck *Check) (CheckResult, []PerfResult) {
	res := CheckResult{ID: ck.ID, Path: ck.Path, Seed: ck.Seed, Claim: claimOf(ck.Rules), Cells: []Cell{},
		Reference: RefInfo{VaryingParts: []string{}}}
	groups := c.groups(ck)

	if ck.Rules.Reference != RefMeasured {
		for _, g := range groups {
			res.Cells = append(res.Cells, c.claimCell(ck, g))
		}
		return res, c.perf(ck, groups, nil)
	}

	var refGroup *group
	if c.refMember != "" && !c.refAmbig {
		for i := range groups {
			if groups[i].member == c.refMember && groups[i].version == c.refVer {
				refGroup = &groups[i]
			}
		}
	}
	var refUsable []cls
	control := ControlUntested
	if refGroup != nil {
		rs := statsOf(*refGroup)
		refUsable = rs.ok
		// a reference run that could not run is excluded and NAMED: it is not "a different output"
		res.Reference.CouldNotRun = rs.cnr
	}
	if len(refUsable) > 0 {
		c.refUsable = true
		res.Reference.Runs = len(refUsable)
		res.Reference.Stable = true
		flag := map[string]bool{}
		// ARGUS-CMP-11 fix F4: the reference is stable on this check iff EVERY PAIR of its usable runs agree. Judged against
		// the first run alone, a non-transitive tolerance made stable / noise depend on which run has the lowest id.
		for i := 0; i < len(refUsable); i++ {
			for j := i + 1; j < len(refUsable); j++ {
				if ok, parts := RecordsAgree(ck.Rules, refUsable[i].recs, refUsable[j].recs); !ok {
					res.Reference.Stable = false
					for _, p := range parts {
						flag[p] = true
					}
				}
			}
		}
		for _, p := range []string{"samples", "status", "headers", "body", "values"} {
			if flag[p] {
				res.Reference.VaryingParts = append(res.Reference.VaryingParts, p)
			}
		}
		switch {
		case !res.Reference.Stable:
			control = ControlUnstable
			res.Reference.Sentence = noiseSentence(distinctClusters(ck.Rules, refUsable), res.Reference.Runs)
		case len(refUsable) >= 2:
			control = ControlStable
		default:
			control = ControlUntested
			c.notes[NoteReferenceSingleRun] = true
			// an untested reference is a gap: its own variation is not on record, so a candidate that equals it
			// proves nothing about the candidate. The cell keeps its own state; the roll-up cannot be same.
			c.refSingle = true
		}
	}

	for _, g := range groups {
		isRef := refGroup != nil && g.member == refGroup.member && g.version == refGroup.version
		res.Cells = append(res.Cells, c.measuredCell(ck, g, isRef, refUsable, res.Reference, control))
	}
	return res, c.perf(ck, groups, refGroup)
}

// noiseSentence is the one closed sentence of a reference that disagrees with itself (design 5).
func noiseSentence(outputs, runs int) string {
	return fmt.Sprintf("The reference gave %d different outputs in %d runs. Mask the field that changes and seal a new set.", outputs, runs)
}

// excludedSentence names runs that could not run and are not in n ("" when none). Closed template.
func excludedSentence(cnr int, inN string) string {
	switch {
	case cnr <= 0:
		return ""
	case cnr == 1:
		return " 1 run could not run and is not counted" + inN + "."
	}
	return fmt.Sprintf(" %d runs could not run and are not counted%s.", cnr, inN)
}

func bases(g group) Cell {
	return Cell{Member: g.member, VersionKey: g.version, PartsDiffer: []string{}}
}

// distinctClusters counts the DISTINCT outputs of the runs (rows, hashes and tolerant values exactly as recorded). It does not
// group runs by tolerance: a tolerance is not transitive, so any grouping would depend on the order the runs are visited in
// (ARGUS-CMP-11 fix F4). Without a tolerance two runs agree exactly when their outputs are equal, so it is the count it always was.
func distinctClusters(rules *Rules, runs []cls) int {
	seen := map[string]bool{}
	for _, r := range runs {
		seen[checkKey(r.recs)] = true
	}
	return len(seen)
}

func checkKey(recs []ScenarioOutput) string {
	rows := sortedRows(recs)
	parts := []string{domainCheck}
	for _, r := range rows {
		vals := ""
		if len(r.Values) > 0 {
			b, _ := json.Marshal(r.Values)
			vals = string(b)
		}
		parts = append(parts, r.Step, "\x1f", strconv.Itoa(r.Sample), "\x1f", r.Hash, "\x1f", vals, "\n")
	}
	return sum(parts...)
}

func cellHash(keys []string) string {
	set := map[string]bool{}
	for _, k := range keys {
		set[k] = true
	}
	var distinct []string
	for k := range set {
		distinct = append(distinct, k)
	}
	sort.Strings(distinct)
	switch len(distinct) {
	case 0:
		return ""
	case 1:
		return distinct[0]
	}
	return sum(domainSet, strings.Join(distinct, ","))
}

func cnrSentence(s groupStats, total int) string {
	switch {
	case s.reasons[rsnSeed] > 0:
		return "Could not run: starting state not established."
	case s.reasons[rsnSet] > 0:
		return "Could not run: the run was of a different set than the comparison."
	case s.reasons[rsnRun] > 0:
		return "Could not run: the run failed before it ran the checks."
	}
	return fmt.Sprintf("Could not run in %d of %d runs: the check errored.", s.cnr, total)
}

const notMeasuredSentence = "Not measured: nothing was recorded for this check."

func (c *calc) measuredCell(ck *Check, g group, isRef bool, refUsable []cls, ref RefInfo, control string) Cell {
	cell := bases(g)
	cell.Reference = isRef
	cell.Control = control
	s := statsOf(g)
	n := len(s.ok)
	cell.Runs, cell.CouldNotRun = n, s.cnr
	total := n + s.cnr + s.unrec

	switch {
	case c.refMember == "" || c.refAmbig || len(refUsable) == 0:
		if isRef && n == 0 && s.cnr > 0 && s.unrec == 0 {
			// the reference member's own run could not run (another set, or it failed): say so, not "no reference output"
			cell.State, cell.Control = StateCouldNotRun, ControlUntested
			cell.Sentence = cnrSentence(s, total)
			return cell
		}
		cell.State, cell.Control = StateNotMeasured, ControlUntested
		cell.Sentence = "Not measured: there is no reference output to compare with."
		return cell
	case n == 0 && s.cnr > 0 && s.unrec == 0:
		cell.State, cell.Sentence = StateCouldNotRun, cnrSentence(s, total)
		return cell
	case s.unrec > 0 || n == 0:
		cell.State, cell.Sentence = StateNotMeasured, notMeasuredSentence
		return cell
	case !ref.Stable:
		cell.State = StateNoise
		cell.Control = ControlUnstable
		// the cell carries what varies in the REFERENCE (not what the candidate did): a difference cannot be told
		// from the reference's own variation, so nothing about the candidate is judged
		cell.PartsDiffer = append([]string{}, ref.VaryingParts...)
		cell.Sentence = ref.Sentence
		return cell
	}

	pct := EffectiveAgreement(ck.Rules.Agreement)
	repRef := refUsable[0].recs
	// ARGUS-CMP-10: the reference's hash covers EVERY usable reference run, as the member's covers every one of its own
	// (cellHash: the one key when they all gave the same output, else a hash over the set of distinct ones). With no
	// tolerance the reference's runs agree only when they are identical, so this is the very hash it was; with one, two
	// runs can agree and still differ, and an approval must not stay standing when the second of them changes.
	var refKeys []string
	for _, u := range refUsable {
		refKeys = append(refKeys, checkKey(u.recs))
	}
	cell.ReferenceHash = cellHash(refKeys)
	var keys []string
	var valuePaths []string
	flag := map[string]bool{}
	cell.Steps = stepNames(repRef)
	if isRef {
		for _, u := range s.ok {
			keys = append(keys, checkKey(u.recs))
		}
		cell.Agreed = n
	} else {
		for _, u := range s.ok {
			keys = append(keys, checkKey(u.recs))
			cell.Steps = unionSorted(cell.Steps, stepNames(u.recs))
			// ARGUS-CMP-11 fix F4: a run agrees only if it agrees with EVERY usable reference run (a tolerance is not
			// transitive, so "agrees with the first one" depends on which run has the lowest id). Without a tolerance the
			// reference runs are identical to one another, and this is the comparison with any one of them it always was.
			agreed := true
			for _, ru := range refUsable {
				if ok, parts := RecordsAgree(ck.Rules, ru.recs, u.recs); !ok {
					agreed = false
					cell.DiffSteps = unionSorted(cell.DiffSteps, differingSteps(ck.Rules, ru.recs, u.recs))
					for _, p := range parts {
						flag[p] = true
					}
					valuePaths = unionSorted(valuePaths, ValueDiffPaths(ck.Rules, ru.recs, u.recs))
				}
			}
			if agreed {
				cell.Agreed++
			}
		}
	}
	cell.MemberHash = cellHash(keys)
	for _, p := range []string{"samples", "status", "headers", "body", "values"} {
		if flag[p] {
			cell.PartsDiffer = append(cell.PartsDiffer, p)
		}
	}
	if flag["values"] {
		cell.ValuePaths = valuePaths
	}
	if float64(cell.Agreed)*100 >= pct*float64(n)-eps {
		cell.State = StateIdentical
	} else {
		cell.State = StateDiffers
	}
	d := n - cell.Agreed
	cell.ClaimSupported = ClaimSupported(d, n, pct)
	cell.Supports = SupportsFor(d, n, pct)
	if !cell.ClaimSupported {
		cell.Unsupported = UnsupportedFor(d, n, pct)
	}
	cell.Short = n < ck.Rules.Repeats || s.pending > 0
	switch {
	case isRef:
		cell.Sentence = fmt.Sprintf("The reference: its %s gave the same output (n = %d).", runsWord(n), n) + excludedSentence(s.cnr, "")
	case cell.State == StateIdentical:
		cell.Sentence = fmt.Sprintf("Agreed in %d of %d runs (n = %d).", cell.Agreed, n, n) + excludedSentence(s.cnr, " in n")
	default:
		cell.Sentence = fmt.Sprintf("Agreed in %d of %d runs (n = %d); differs in: %s.", cell.Agreed, n, n, strings.Join(cell.PartsDiffer, ", ")) + excludedSentence(s.cnr, " in n")
	}
	return cell
}

func (c *calc) claimCell(ck *Check, g group) Cell {
	cell := bases(g)
	cell.Control = ControlUntested
	s := statsOf(g)
	var outcomes []string
	for _, k := range s.ok {
		outcomes = append(outcomes, k.outcome)
	}
	j := JudgeClaim(outcomes, ck.Rules.Agreement)
	n := j.Runs
	cell.Runs, cell.Agreed, cell.CouldNotRun = n, j.Held, s.cnr+j.CouldNotRun
	total := n + cell.CouldNotRun + s.unrec
	switch {
	case n == 0 && s.cnr > 0 && s.unrec == 0:
		cell.State, cell.Sentence = StateCouldNotRun, cnrSentence(s, total)
		return cell
	case s.unrec > 0 || n == 0:
		cell.State, cell.Sentence = StateNotMeasured, notMeasuredSentence
		return cell
	}
	cell.State = j.State
	if cell.State == StateDiffers {
		cell.Steps, cell.DiffSteps = []string{""}, []string{""} // a claim has no steps: it is judged from the check's own outcome
	}
	cell.ClaimSupported, cell.Supports, cell.Unsupported = j.ClaimSupported, j.Supports, j.Unsupported
	cell.Short = n < ck.Rules.Repeats || s.pending > 0
	cell.MemberHash = sum(domainClaim, strconv.Itoa(j.Held), "/", strconv.Itoa(n))
	verb := "Matches the expected output"
	if ck.Rules.Reference == RefProperty {
		verb = "The property held"
	}
	if cell.State == StateIdentical {
		cell.Sentence = fmt.Sprintf("%s in %d of %d runs (n = %d).", verb, j.Held, n, n) + excludedSentence(cell.CouldNotRun, " in n")
	} else {
		cell.Sentence = fmt.Sprintf("%s in only %d of %d runs (n = %d); %s%% were declared.", verb, j.Held, n, n,
			strconv.FormatFloat(EffectiveAgreement(ck.Rules.Agreement), 'f', -1, 64)) + excludedSentence(cell.CouldNotRun, " in n")
	}
	return cell
}

// ── performance (Not Worse Than) ────────────────────────────────────────────────────────────────

func metricOf(l *LoadNumbers, metric string) float64 {
	switch metric {
	case "p50":
		return l.P50Ms
	case "p95":
		return l.P95Ms
	case "p99":
		return l.P99Ms
	}
	return l.ErrorRate
}

func (c *calc) loadsOf(ck *Check, g group, metric string) []float64 {
	var vals []float64
	for _, k := range g.runs {
		r := k.run
		if runPhase(r) != phaseRan || r.SetHash != c.in.SetHash || (!ck.Seed && c.seedFailed(r)) {
			continue
		}
		// fix F7: a run in which the load check FAILED (its own absolute threshold breached, say) still contributes its
		// numbers: they are what the system did. A run that ERRORED on the check (or reports an outcome this build does not
		// know, which classify also counts as could not run) did not measure, contributes none, and is counted in could_not_run.
		if o, has := r.Outcomes[ck.ID]; has && o != OutcomePassed && o != OutcomeFailed && o != OutcomeDegraded {
			continue
		}
		var rows []ScenarioOutput
		for _, row := range r.Outputs {
			if row.ScenarioID == ck.ID {
				rows = append(rows, row)
			}
		}
		for _, row := range sortedRows(rows) {
			// a record with no samples measured nothing: its 0 ms is not "faster", it is no number at all
			// (fix F3) a record whose percentiles are out of order is no number either: a row stored before the ordering was
			// enforced reads as not measured, like a dropped one
			if row.Load != nil && row.Load.Samples > 0 && loadNumbersOK(row.Load) {
				vals = append(vals, metricOf(row.Load, metric))
				break
			}
		}
	}
	return vals
}

func (c *calc) perf(ck *Check, groups []group, measuredRef *group) []PerfResult {
	if len(ck.Rules.NotWorseThan) == 0 {
		return nil
	}
	refGroup := measuredRef
	if refGroup == nil && c.refMember != "" && !c.refAmbig {
		for i := range groups {
			if groups[i].member == c.refMember && groups[i].version == c.refVer {
				refGroup = &groups[i]
			}
		}
	}
	var out []PerfResult
	for _, b := range ck.Rules.NotWorseThan {
		pr := PerfResult{Check: ck.ID, Metric: b.Metric, Band: strconv.FormatFloat(b.Value, 'f', -1, 64) + b.Unit, Cells: []PerfCell{}}
		var refVals []float64
		if refGroup != nil {
			refVals = c.loadsOf(ck, *refGroup, b.Metric)
		}
		pr.Reference = PerfValue{Value: Median(refVals), Runs: len(refVals)}
		for _, g := range groups {
			if refGroup != nil && g.member == refGroup.member && g.version == refGroup.version {
				continue
			}
			vals := c.loadsOf(ck, g, b.Metric)
			j := JudgeBand(b, refVals, vals)
			pr.Cells = append(pr.Cells, PerfCell{Member: g.member, VersionKey: g.version, Value: j.Member, Runs: j.MemberRuns, State: j.State})
		}
		out = append(out, pr)
	}
	return out
}
