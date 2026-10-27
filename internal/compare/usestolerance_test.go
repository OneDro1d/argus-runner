package compare

import "testing"

// AC-F3: UsesTolerance answers "does any rule of the set carry a Tolerance line" — the data-access question — on its own,
// never through the executor-floor table. Lowering the key's floor (KeyFloors) must not switch the Limited refusals off.
func TestUsesTolerance_IsIndependentOfTheExecutorFloor(t *testing.T) {
	with := []*Rules{nil, {}, {Tolerance: []Tolerance{{Path: "$.total", Kind: "abs", Value: 0.01}}}}
	without := []*Rules{nil, {}, {Mask: []string{"$.id"}}}
	if !UsesTolerance(with) {
		t.Fatalf("UsesTolerance(a set with a Tolerance rule) = false")
	}
	if UsesTolerance(without) || UsesTolerance(nil) {
		t.Fatalf("UsesTolerance(a set without one) = true")
	}
	prev := KeyFloors["Tolerance"]
	KeyFloors["Tolerance"] = FloorE1
	defer func() { KeyFloors["Tolerance"] = prev }()
	if !UsesTolerance(with) {
		t.Fatalf("UsesTolerance turned false when the Tolerance floor was lowered to E1: it must not read the floor table")
	}
	// KeysNeedingE2 still reads the floor (that is its job): with the floor at E1 it no longer names Tolerance
	for _, k := range KeysNeedingE2(with) {
		if k == "Tolerance" {
			t.Fatalf("KeysNeedingE2 named Tolerance with its floor at E1")
		}
	}
	KeyFloors["Tolerance"] = FloorE2
	if got := KeysNeedingE2(with); len(got) != 1 || got[0] != "Tolerance" {
		t.Fatalf("KeysNeedingE2 with the floor at E2 = %v, want [Tolerance]", got)
	}
}
