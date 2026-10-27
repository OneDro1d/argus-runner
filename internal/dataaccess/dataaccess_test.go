package dataaccess

import "testing"

// AC-F3: the two values are spelled ONCE. The store, the signed claim, the Principal and the web API all carry these
// exact strings, so a change here is a wire and schema change (migration 083's CHECK lists the same two).
func TestValues_AreTheTwoSpelledByTheSpec(t *testing.T) {
	if Full != "full" || Limited != "limited" {
		t.Fatalf("Full=%q Limited=%q, want \"full\" and \"limited\" (the spec's field values, migration 083)", Full, Limited)
	}
}

// Valid accepts exactly the two values: an empty or a differently spelled value is not a type.
func TestValid_ExactlyTheTwoValues(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"full", true},
		{"limited", true},
		{"", false},
		{"Full", false},
		{"LIMITED", false},
		{" full", false},
		{"full ", false},
		{"none", false},
		{"author", false},
	} {
		if got := Valid(tc.in); got != tc.want {
			t.Errorf("Valid(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// IsFull is the fail-closed reading: only the exact value "full" is Full. A credential whose type was never set (empty)
// or carries anything else reads as NOT Full, so new code that forgets to set the type can never serve app data.
func TestIsFull_FailsClosed(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"full", true},
		{"limited", false},
		{"", false},
		{"Full", false},
		{"full ", false},
		{"anything", false},
	} {
		if got := IsFull(tc.in); got != tc.want {
			t.Errorf("IsFull(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// Parse is the request-side reading (POST /api/tokens, the OAuth allow page): an absent value means the default, Full
// (the spec's 2: "For an author token it defaults to full"); a value that is not one of the two is refused, never
// guessed.
func TestParse_DefaultsToFullAndRefusesTheUnknown(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", Full, false},
		{"full", Full, false},
		{"limited", Limited, false},
		{"Limited", "", true},
		{"partial", "", true},
		{" ", "", true},
	} {
		got, err := Parse(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("Parse(%q) = %q, %v; want %q, error=%v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}
