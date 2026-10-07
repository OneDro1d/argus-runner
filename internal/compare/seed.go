package compare

import "sort"

// SeedTag is the one tag that makes a check a SEED check (ARGUS-CMP-7): the exact, case-sensitive
// word `seed` among the check's `**Tags**`. `seeded`, `Seed` and `seed-1` are ordinary tags.
const SeedTag = "seed"

// IsSeed reports whether the tags carry the exact tag `seed`.
func IsSeed(tags []string) bool {
	for _, t := range tags {
		if t == SeedTag {
			return true
		}
	}
	return false
}

// SetCheck is one check of a set about to be compared, as the seed rule reads it: its ID (the order the executor
// runs the set in), whether it is a seed, and whether it declares a valid `## COMPARE`.
type SetCheck struct {
	ID       string
	Seed     bool
	Compared bool
}

// HasMeasured reports whether any check's rules is `measured` (nil entries are checks that declare no `## COMPARE`).
// A comparison with a measured check needs a reference member and keeps the reference-first gate; one without does not.
func HasMeasured(rules []*Rules) bool {
	for _, r := range rules {
		if r != nil && r.Reference == RefMeasured {
			return true
		}
	}
	return false
}

// SeedRefusal applies the seed rule to a set at comparison creation: "" when it holds, else one closed sentence that
// names the check ids concerned (the author's own, as every refusal does) and says how to fix it. It reads ids and
// tags only, never a body.
//
//   - Every seed check's ID must sort BEFORE every non-seed ID of the set, in byte order. The executor runs a set in ID
//     order (scenario.DiscoverFiles sorts by ID), so this is what makes the seed run first. The refusal names the
//     lowest-sorting non-seed ID and the lowest seed ID that sorts after it.
//   - A set whose every check is a seed is refused: nothing would be compared after the starting state.
//   - A set in which no check but a seed declares `## COMPARE` is refused for the same reason: a seed's own cell is
//     shown but is never counted, so the roll-up would have nothing to judge and could never be anything but incomplete.
func SeedRefusal(set []SetCheck) string {
	var seeds, others []string
	otherCompared := false
	for _, c := range set {
		if c.Seed {
			seeds = append(seeds, c.ID)
			continue
		}
		others = append(others, c.ID)
		if c.Compared {
			otherCompared = true
		}
	}
	if len(seeds) == 0 {
		return ""
	}
	sort.Strings(seeds)
	sort.Strings(others)
	if len(others) == 0 {
		return "every check of the set is tagged seed (for example " + seeds[0] + "), so nothing is compared after the starting state is established -- add the checks to compare, which are not tagged seed"
	}
	for _, s := range seeds {
		if s > others[0] {
			return "seed check " + s + " sorts after check " + others[0] + ": the executor runs a set in ID order (byte order), so " + others[0] +
				" would run before the seed -- rename the seed check so that its ID sorts before every other check of the set (for example 00-SEED-...)"
		}
	}
	if !otherCompared {
		return "no check other than the seed checks declares a ## COMPARE section, so nothing is compared after the starting state is established"
	}
	return ""
}
