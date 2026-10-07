package certificate

// integrity_test.go — (tallies and finished_at were forgeable) and
// (verify's reason text for an anchor read through an --rpc that serves another chain). Every test
// drives Verify/Overall/Render against a in-memory test chain that anchored the payload — the same
// path `argus certificate verify` takes — never a hand-built result.

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/chainread"
)

func verifyAll(t *testing.T, cert *Certificate, svc chainread.ChainService) ([]CheckResult, bool) {
	t.Helper()
	results := Verify(context.Background(), cert, svc, nil, nil)
	for _, l := range Render(cert, results) {
		t.Log(l)
	}
	return results, Overall(cert, results)
}

func wantNotVerifiedNamed(t *testing.T, cert *Certificate, results []CheckResult, overall bool, name string) {
	t.Helper()
	if overall {
		t.Errorf("Overall = true, want false")
	}
	if r := resultNamed(t, results, name); r.Status != StatusMismatch || r.Detail == "" {
		t.Errorf("%s = %s %q, want a mismatch with a named reason", name, r.Status, r.Detail)
	}
}

// ── item 1a: a certificate ALREADY issued in today's (v1) format ──

func TestV1_HonestCertificate_StillVerifies_AndSaysWhatIsNotAnchored(t *testing.T) {
	cert, svc, _, _ := verifyFixture(t)
	results, overall := verifyAll(t, cert, svc)
	if !overall {
		t.Fatalf("an honest v1 certificate no longer verifies")
	}
	un := strings.Join(Unanchored(cert), "\n")
	for _, want := range []string{"tallies", "failed/errored split", "finished_at"} {
		if !strings.Contains(un, want) {
			t.Errorf("Unanchored omits %q:\n%s", want, un)
		}
	}
	found := false
	for _, l := range Render(cert, results) {
		found = found || strings.HasPrefix(l, "NOT ANCHORED: finished_at")
	}
	if !found {
		t.Errorf("the printed verify output has no NOT ANCHORED line for finished_at")
	}
}

func TestV1_ForgedTallies_AreRefused(t *testing.T) {
	cases := []struct {
		name string
		edit func(c *Certificate)
	}{
		{"passed=250 (scenario_count 3)", func(c *Certificate) { c.Tallies.Passed = 250 }},
		{"failed=1 on a passed verdict", func(c *Certificate) { c.Tallies.Failed = 1; c.Tallies.Passed = 2 }},
		{"errored=1 on a passed verdict", func(c *Certificate) { c.Tallies.Errored = 1; c.Tallies.Passed = 2 }},
		{"passed short of scenario_count on a passed verdict", func(c *Certificate) { c.Tallies.Passed = 2 }},
		{"a degraded bucket a v1 certificate does not have", func(c *Certificate) { c.Tallies.Degraded = 1; c.Tallies.Passed = 2 }},
		{"negative counts that still sum to scenario_count", func(c *Certificate) { c.Tallies.Passed = 5; c.Tallies.Failed = -2 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cert, svc, _, _ := verifyFixture(t)
			tc.edit(cert)
			results, overall := verifyAll(t, cert, svc)
			wantNotVerifiedNamed(t, cert, results, overall, "certificate tallies")
		})
	}
}

func TestV1_VerdictFailedWithNoFailure_IsRefused(t *testing.T) {
	cert, svc, _, _ := buildVerifyFixtureFor(t, false, "failed", Tallies{Passed: 3}) // chain says failed, tallies say all passed
	results, overall := verifyAll(t, cert, svc)
	wantNotVerifiedNamed(t, cert, results, overall, "certificate tallies")
}

func TestV1_HonestFailedAndDegradedRuns_StillVerify(t *testing.T) {
	// failed: 1 failed + 1 errored + 1 passed. Also a failed run that had a degraded scenario: a v1
	// certificate cannot show the degraded one, so passed+failed+errored is BELOW scenario_count and
	// must not be refused for it.
	for name, tl := range map[string]Tallies{
		"failed":                   {Passed: 1, Failed: 1, Errored: 1},
		"failed beside a degraded": {Passed: 1, Failed: 1},
	} {
		t.Run(name, func(t *testing.T) {
			cert, svc, _, _ := buildVerifyFixtureFor(t, false, "failed", tl)
			if _, overall := verifyAll(t, cert, svc); !overall {
				t.Errorf("an honest %s v1 certificate was refused", name)
			}
		})
	}
	t.Run("degraded", func(t *testing.T) {
		cert, svc, _, _ := buildVerifyFixtureFor(t, false, "degraded", Tallies{Passed: 2}) // 1 degraded scenario is not in a v1 bucket
		if _, overall := verifyAll(t, cert, svc); !overall {
			t.Errorf("an honest degraded v1 certificate was refused")
		}
	})
	t.Run("degraded with nothing degraded", func(t *testing.T) {
		cert, svc, _, _ := buildVerifyFixtureFor(t, false, "degraded", Tallies{Passed: 3})
		results, overall := verifyAll(t, cert, svc)
		wantNotVerifiedNamed(t, cert, results, overall, "certificate tallies")
	})
}

func TestV1_FinishedAtIsNotAnchored_SoOnlyTheNoticeProtectsIt(t *testing.T) {
	// The honest limit of a v1 certificate, pinned so nobody reads "verified" as covering it: moving
	// finished_at does NOT fail a v1 verify — the output says the field is not anchored instead.
	cert, svc, _, _ := verifyFixture(t)
	cert.FinishedAt = "2026-01-01T00:00:00Z"
	results, overall := verifyAll(t, cert, svc)
	if !overall {
		t.Fatalf("a v1 certificate with a moved finished_at was refused; v1 cannot know")
	}
	if !strings.Contains(strings.Join(Render(cert, results), "\n"), "NOT ANCHORED: finished_at") {
		t.Errorf("verify output does not say finished_at is not anchored")
	}
}

// ── item 1b: NEW certificates anchor tallies and finished_at ──

func TestV2_HonestCertificate_Verifies(t *testing.T) {
	cert, svc, _, _ := verifyFixtureTallied(t)
	if cert.Format != FormatV2 {
		t.Fatalf("fixture format = %q", cert.Format)
	}
	results, overall := verifyAll(t, cert, svc)
	if !overall {
		t.Fatalf("an honest v2 certificate did not verify: %+v", results)
	}
	for _, l := range Unanchored(cert) {
		if strings.Contains(l, "finished_at") || strings.Contains(l, "tallies") {
			t.Errorf("a v2 certificate claims %q is not anchored", l)
		}
	}
}

func TestV2_ForgedTalliesAndFinishedAt_FailTheVerdictAnchor(t *testing.T) {
	// A verdict the chain holds as FAILED with 2 passed + 1 failed. The forgeries below all stay
	// internally consistent with the verdict and scenario_count, so checkTallies passes them and ONLY the
	// anchored payload can refuse them — which is the point of anchoring.
	cases := []struct {
		name string
		edit func(c *Certificate)
	}{
		{"failed 1 -> 2 passed 2 -> 1", func(c *Certificate) { c.Tallies.Failed, c.Tallies.Passed = 2, 1 }},
		{"failed 1 -> errored 1", func(c *Certificate) { c.Tallies.Failed, c.Tallies.Errored = 0, 1 }},
		{"finished_at moved to January", func(c *Certificate) { c.FinishedAt = "2026-01-05T00:00:00Z" }},
		{"finished_at blanked", func(c *Certificate) { c.FinishedAt = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cert, svc, _, _ := buildVerifyFixtureFor(t, true, "failed", Tallies{Passed: 2, Failed: 1})
			if _, overall := verifyAll(t, cert, svc); !overall {
				t.Fatalf("the honest certificate does not verify")
			}
			tc.edit(cert)
			results, overall := verifyAll(t, cert, svc)
			if overall {
				t.Errorf("Overall = true for a forged v2 certificate")
			}
			if r := resultNamed(t, results, "anchor v3 "+verifyChainName); r.Status != StatusMismatch {
				t.Errorf("verdict anchor = %s, want mismatch: the chain holds the true tallies/finished_at", r.Status)
			}
		})
	}
}

func TestV2_PassedEqualsTwoFiftyIsRefused(t *testing.T) {
	cert, svc, _, _ := verifyFixtureTallied(t)
	cert.Tallies.Passed = 250
	results, overall := verifyAll(t, cert, svc)
	wantNotVerifiedNamed(t, cert, results, overall, "certificate tallies")
	if r := resultNamed(t, results, "anchor v3 "+verifyChainName); r.Status != StatusMismatch {
		t.Errorf("verdict anchor = %s, want mismatch as well", r.Status)
	}
}

func TestV2_DegradedRun_VerifiesAndIsAnchored(t *testing.T) {
	cert, svc, _, _ := buildVerifyFixtureFor(t, true, "degraded", Tallies{Passed: 2, Degraded: 1})
	if _, overall := verifyAll(t, cert, svc); !overall {
		t.Fatalf("an honest degraded v2 certificate was refused")
	}
	cert.Tallies.Degraded = 0
	cert.Tallies.Passed = 3
	results, overall := verifyAll(t, cert, svc)
	wantNotVerifiedNamed(t, cert, results, overall, "certificate tallies")
}

func TestRelabelling_AcrossFormats_DoesNotVerify(t *testing.T) {
	t.Run("a v2 certificate relabelled v1", func(t *testing.T) {
		cert, svc, _, _ := verifyFixtureTallied(t)
		cert.Format = FormatV1
		cert.Anchors[0].ChainID, cert.Anchors[1].ChainID = 0, 0 // a v1 document has no chain ids
		results, overall := verifyAll(t, cert, svc)
		if overall {
			t.Errorf("Overall = true: the chain holds the tallied payload, the v1 rebuild must not match")
		}
		if r := resultNamed(t, results, "anchor v3 "+verifyChainName); r.Status != StatusMismatch {
			t.Errorf("verdict anchor = %s, want mismatch", r.Status)
		}
	})
	t.Run("a v1 certificate relabelled v2", func(t *testing.T) {
		cert, svc, _, _ := verifyFixture(t)
		cert.Format = FormatV2
		results, overall := verifyAll(t, cert, svc)
		if overall {
			t.Errorf("Overall = true: the chain holds the legacy payload, the v2 rebuild must not match")
		}
		if r := resultNamed(t, results, "anchor v3 "+verifyChainName); r.Status != StatusMismatch {
			t.Errorf("verdict anchor = %s, want mismatch", r.Status)
		}
	})
}

func TestUnknownFormat_IsRefusedByName(t *testing.T) {
	cert, svc, _, _ := verifyFixtureTallied(t)
	cert.Format = "argus-certificate/v9"
	results, overall := verifyAll(t, cert, svc)
	wantNotVerifiedNamed(t, cert, results, overall, "certificate format")
}

func TestVerdictPayloads_AreDeterministicJSON(t *testing.T) {
	cert, _, _, _ := verifyFixtureTallied(t)
	a, err1 := verdictPayload(cert)
	b, err2 := verdictPayload(cert)
	if err1 != nil || err2 != nil || string(a) != string(b) {
		t.Fatalf("verdictPayload not stable: %q vs %q (%v %v)", a, b, err1, err2)
	}
	const want = `{"run_id":"run-verify-1","artifact_digest":"sha256:0000000000000000000000000000000000000000000000000000000000000002","evidence_bundle_hash":"sha256:0000000000000000000000000000000000000000000000000000000000000003","verdict":"passed","tallies":{"passed":3,"failed":0,"errored":0,"degraded":0},"finished_at":"2026-09-30T08:15:00Z"}`
	if string(a) != want {
		t.Errorf("tallied payload bytes changed — every issued v2 certificate depends on them:\n got: %s\nwant: %s", a, want)
	}
	cert.Format = FormatV1
	old, _ := verdictPayload(cert)
	const wantOld = `{"run_id":"run-verify-1","artifact_digest":"sha256:0000000000000000000000000000000000000000000000000000000000000002","evidence_bundle_hash":"sha256:0000000000000000000000000000000000000000000000000000000000000003","verdict":"passed"}`
	if string(old) != wantOld {
		t.Errorf("legacy payload bytes changed — every issued v1 certificate depends on them:\n got: %s\nwant: %s", old, wantOld)
	}
}

// ── item 3: an anchor read through an --rpc that serves another chain ──

// prunedRPC is a node for a DIFFERENT chain: every block read fails the way a Base Sepolia node does
// for a block number that only exists on the PoA chain, and it reports the chain id it serves.
type prunedRPC struct {
	chainread.ChainService
	id  *big.Int
	err error
}

func (p prunedRPC) ReadBlock(int64) ([]chainread.BlockTx, error) { return nil, p.err }
func (p prunedRPC) ChainID(context.Context) (*big.Int, error) {
	if p.id == nil {
		return nil, errors.New("eth_chainId unavailable")
	}
	return p.id, nil
}

const prunedMsg = "pruned history unavailable: requested 1103038, earliest available 46000000"

func TestWrongChainRPC_NamesBothChains_StatusStaysUnreachable(t *testing.T) {
	other := newFakeChain()
	t.Run("v2 certificate: both chain ids known", func(t *testing.T) {
		cert, _, _, _ := verifyFixtureTallied(t)
		svc := prunedRPC{ChainService: other, id: big.NewInt(84532), err: errors.New(prunedMsg)}
		results, overall := verifyAll(t, cert, svc)
		if overall {
			t.Errorf("Overall = true with every anchor unreadable")
		}
		for _, r := range anchorResults(cert, results) {
			if r.Status != StatusUnreachable {
				t.Errorf("%s: status %s, want unreachable (not mismatch)", r.Name, r.Status)
			}
			for _, want := range []string{`"` + verifyChainName + `"`, "chain id 2026", "84532", prunedMsg} {
				if !strings.Contains(r.Detail, want) {
					t.Errorf("%s: detail lacks %q: %s", r.Name, want, r.Detail)
				}
			}
		}
	})
	t.Run("v1 certificate: the anchor's chain id is unknown, the chain is still named", func(t *testing.T) {
		cert, _, _, _ := verifyFixture(t)
		svc := prunedRPC{ChainService: other, id: big.NewInt(84532), err: errors.New(prunedMsg)}
		results, _ := verifyAll(t, cert, svc)
		for _, r := range anchorResults(cert, results) {
			if r.Status != StatusUnreachable {
				t.Errorf("%s: status %s, want unreachable", r.Name, r.Status)
			}
			for _, want := range []string{`"` + verifyChainName + `"`, "84532", "does not record", prunedMsg} {
				if !strings.Contains(r.Detail, want) {
					t.Errorf("%s: detail lacks %q: %s", r.Name, want, r.Detail)
				}
			}
		}
	})
	t.Run("the RPC cannot say its chain id: the anchor's chain is still named", func(t *testing.T) {
		cert, _, _, _ := verifyFixtureTallied(t)
		svc := prunedRPC{ChainService: other, err: errors.New(prunedMsg)}
		results, _ := verifyAll(t, cert, svc)
		for _, r := range anchorResults(cert, results) {
			if r.Status != StatusUnreachable || !strings.Contains(r.Detail, `"`+verifyChainName+`"`) || !strings.Contains(r.Detail, prunedMsg) {
				t.Errorf("%s: %s %s", r.Name, r.Status, r.Detail)
			}
		}
	})
	t.Run("same chain id: the failure is not blamed on the endpoint", func(t *testing.T) {
		cert, _, _, _ := verifyFixtureTallied(t)
		svc := prunedRPC{ChainService: other, id: big.NewInt(2026), err: errors.New(prunedMsg)}
		results, _ := verifyAll(t, cert, svc)
		for _, r := range anchorResults(cert, results) {
			if !strings.Contains(r.Detail, "same chain id") {
				t.Errorf("%s: detail = %s", r.Name, r.Detail)
			}
		}
	})
}

func TestWorkingRPC_ThatReportsItsChainID_StillVerifies(t *testing.T) {
	cert, svc, _, _ := verifyFixtureTallied(t)
	// same service, plus a ChainID method: asking the id must not change a successful read.
	type withID struct {
		chainread.ChainService
		idOnly
	}
	if _, overall := verifyAll(t, cert, withID{ChainService: svc, idOnly: idOnly{big.NewInt(2026)}}); !overall {
		t.Errorf("a working --rpc that reports its chain id no longer verifies")
	}
}

type idOnly struct{ id *big.Int }

func (i idOnly) ChainID(context.Context) (*big.Int, error) { return i.id, nil }
