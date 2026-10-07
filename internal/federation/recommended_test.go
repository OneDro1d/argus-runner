package federation

import (
	"strings"
	"testing"
)

// VR-F18: the control plane publishes a digest-pinned recommended image PER VARIANT, so onboarding
// can ask "I need full — what digest?" and get a pinned answer.
//
// WHY PER VARIANT AT ALL: onboarding derives the variant from the SUT's argus-config (a
// non-PostgreSQL JDBC driver needs `full`, because slim ships only the PostgreSQL driver). One
// recommendation cannot serve both, and the failure is silent — a full-needing SUT handed a slim
// executor passes onboarding and its database scenarios fail later looking like the SUT's fault.
// That is INT-034.

const slimRef = "ghcr.io/onedro1d/argus-runner@sha256:1111111111111111111111111111111111111111111111111111111111111111"
const fullRef = "ghcr.io/onedro1d/argus-runner@sha256:2222222222222222222222222222222222222222222222222222222222222222"

func TestRecommendedFor_PerVariant(t *testing.T) {
	v := VersionInfo{RecommendedImages: map[string]string{"slim": slimRef, "full": fullRef}}
	if got := v.RecommendedFor("slim"); got != slimRef {
		t.Errorf("slim = %q", got)
	}
	if got := v.RecommendedFor("full"); got != fullRef {
		t.Errorf("full = %q", got)
	}
	// An unspecified variant is the default image, not an error: slim IS the default.
	if got := v.RecommendedFor(""); got != slimRef {
		t.Errorf("empty variant = %q, want the slim recommendation", got)
	}
}

// THE RULE THAT MATTERS. The legacy single-value RecommendedImage answers for slim ONLY. Letting it
// answer for `full` would hand a full-needing SUT a slim executor — precisely the defect VR-F18
// exists to stop, reintroduced by a well-meaning fallback.
func TestRecommendedFor_LegacyValueNeverAnswersForFull(t *testing.T) {
	v := VersionInfo{RecommendedImage: slimRef} // a control plane configured before M3-FX
	if got := v.RecommendedFor("slim"); got != slimRef {
		t.Errorf("the legacy value must still answer for slim during the cutover; got %q", got)
	}
	if got := v.RecommendedFor("full"); got != "" {
		t.Errorf("the legacy value answered for FULL with %q — a full-needing SUT would silently get a slim executor", got)
	}
}

// "No recommendation for this variant" must stay distinguishable from "here is a guess". Empty is
// the honest answer, and the caller reports it rather than substituting a tag.
func TestRecommendedFor_UnknownVariantIsEmptyNotAGuess(t *testing.T) {
	v := VersionInfo{RecommendedImages: map[string]string{"slim": slimRef}}
	if got := v.RecommendedFor("full"); got != "" {
		t.Errorf("full = %q, want empty — the control plane has no recommendation for it", got)
	}
	if got := v.RecommendedFor("nonsense"); got != "" {
		t.Errorf("unknown variant = %q, want empty", got)
	}
}

// A moving tag defeats the whole point: the promise is "you and I get the same executor", and only a
// digest can keep it. Surfaced as a startup warning rather than a refusal — refusing to start a
// control plane over this would be worse than running with a named flaw.
func TestUnpinnedRecommendations_FlagsTags(t *testing.T) {
	v := VersionInfo{
		RecommendedImage:  "ghcr.io/onedro1d/argus-runner:m3-dev", // a TAG
		RecommendedImages: map[string]string{"slim": slimRef, "full": "ghcr.io/x/y:full"},
	}
	bad := v.UnpinnedRecommendations()
	if len(bad) != 2 {
		t.Fatalf("UnpinnedRecommendations = %v, want the two tag-based values", bad)
	}
	joined := strings.Join(bad, " ")
	if !strings.Contains(joined, "recommended_image=") || !strings.Contains(joined, "recommended_images.full=") {
		t.Errorf("the warning must name WHICH values are unpinned; got %v", bad)
	}
	if strings.Contains(joined, "recommended_images.slim") {
		t.Error("a digest-pinned value must not be reported as unpinned")
	}
}

func TestUnpinnedRecommendations_AllPinnedIsSilent(t *testing.T) {
	v := VersionInfo{RecommendedImages: map[string]string{"slim": slimRef, "full": fullRef}}
	if bad := v.UnpinnedRecommendations(); len(bad) != 0 {
		t.Errorf("UnpinnedRecommendations = %v, want none", bad)
	}
}

// Nothing configured is not a warning about pinning — it is a different condition (no recommendation
// at all), reported separately at startup. Conflating them would tell an operator to fix a value
// they never set.
func TestUnpinnedRecommendations_EmptyIsNotUnpinned(t *testing.T) {
	if bad := (VersionInfo{}).UnpinnedRecommendations(); len(bad) != 0 {
		t.Errorf("UnpinnedRecommendations = %v, want none for an unconfigured control plane", bad)
	}
}
