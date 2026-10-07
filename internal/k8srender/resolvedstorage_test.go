package k8srender

import "testing"

// T2.2: LocalTier and ResolvedStorage are the two small exports cmdRenderK8s needs — the managed-tier
// warning condition and "what will this instance actually render with" — without keeping a second
// copy of normalize()'s tier mapping. Pinned here so a change to either delegates correctly rather
// than drifting from normalize() the way StorageDefaultsFor's own doc comment warns against.

func TestLocalTier(t *testing.T) {
	for _, tc := range []struct {
		tier string
		want bool
	}{
		{"k3d", true}, {"kind", true}, {"minikube", true},
		{"K3D", true}, {" k3d ", true}, // case/whitespace-insensitive, like normalize()'s own lookup
		{"aks", false}, {"managed", false}, {"eks", false}, {"gke", false}, {"", false},
	} {
		if got := LocalTier(tc.tier); got != tc.want {
			t.Errorf("LocalTier(%q) = %v, want %v", tc.tier, got, tc.want)
		}
	}
}

// TestResolvedStorage_UnsetMatchesStorageDefaultsFor: with StorageClass/AccessMode both empty,
// ResolvedStorage must agree with StorageDefaultsFor(tier) exactly — the same tier mapping, asked
// two different ways.
func TestResolvedStorage_UnsetMatchesStorageDefaultsFor(t *testing.T) {
	for _, tier := range []string{"aks", "k3d", "kind", "minikube", "managed", "eks", "gke", ""} {
		in := Instance{Tier: tier}
		wantClass, wantMode := StorageDefaultsFor(tier)
		gotClass, gotMode := in.ResolvedStorage()
		if gotClass != wantClass || gotMode != wantMode {
			t.Errorf("tier %q: ResolvedStorage() = (%q, %q), want (%q, %q)", tier, gotClass, gotMode, wantClass, wantMode)
		}
	}
}

// TestResolvedStorage_RespectsExplicitOverride: unlike StorageDefaultsFor (which only ever answers
// for an UNSET instance), ResolvedStorage must report an operator-supplied override verbatim, even
// on a tier whose default would otherwise be something else entirely.
func TestResolvedStorage_RespectsExplicitOverride(t *testing.T) {
	in := Instance{Tier: "managed", StorageClass: "my-rwx-class", AccessMode: "ReadWriteMany"}
	gotClass, gotMode := in.ResolvedStorage()
	if gotClass != "my-rwx-class" || gotMode != "ReadWriteMany" {
		t.Errorf("ResolvedStorage() = (%q, %q), want (\"my-rwx-class\", \"ReadWriteMany\") — an explicit override must not be replaced by the tier default", gotClass, gotMode)
	}
}

// TestResolvedStorage_DoesNotMutateCaller: ResolvedStorage has a value receiver, so calling it must
// never leave the caller's own Instance normalized — a caller that later checks `in.StorageClass ==
// ""` to decide whether an override was given would otherwise get a false answer.
func TestResolvedStorage_DoesNotMutateCaller(t *testing.T) {
	in := Instance{Tier: "aks"}
	_, _ = in.ResolvedStorage()
	if in.StorageClass != "" || in.AccessMode != "" {
		t.Errorf("ResolvedStorage() mutated the caller's Instance: StorageClass=%q AccessMode=%q, want both still empty", in.StorageClass, in.AccessMode)
	}
}
