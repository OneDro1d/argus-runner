package router

// V27-009 redesign (V29-02 §1.4.T1): the author token minted during onboarding lives in ONE place on a machine —
// a CloudRecord per (control plane, user account) — and a test folder only REFERS to its record. Folder Cloud
// entries used to hold a copy of the token each, plus a machine-level copy that every wire and unwire dropped
// (the defect this row was filed for); with a single record there is nothing to keep in sync and nothing to lose.

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// CloudRecord is the machine's record of one (control plane, user) pair: where the author plane is, whose
// account it is, this machine's id under that registration, and the ONE copy of the token.
type CloudRecord struct {
	URL      string    `json:"url"`
	User     string    `json:"user,omitempty"`      // "" only during the legacy lift (LoadState); filled from the first heartbeat's `owner`
	RouterID string    `json:"router_id,omitempty"` // routerIDFor(pubkey) — one per machine, repeated per record
	Token    string    `json:"token,omitempty"`     // the author token minted during onboarding — THE ONLY COPY on this machine
	Expires  time.Time `json:"expires,omitempty"`
}

// CloudRef is what a FOLDER carries: which record it uses. Never a token.
type CloudRef struct {
	URL  string `json:"url"`
	User string `json:"user,omitempty"` // "" resolves to the sole record for URL (legacy lift; single-user machine)
}

func sameURL(a, b string) bool { return strings.TrimRight(a, "/") == strings.TrimRight(b, "/") }

// RecordFor returns the record for (url, user). user == "" matches the SOLE record for url; with several records
// for one url and no user, nothing matches (the caller must say whose).
func (s *State) RecordFor(url, user string) *CloudRecord {
	var sole *CloudRecord
	n := 0
	for i := range s.Clouds {
		r := &s.Clouds[i]
		if !sameURL(r.URL, url) {
			continue
		}
		if user != "" {
			if r.User == user {
				return r
			}
			continue
		}
		n++
		sole = r
	}
	if user == "" && n == 1 {
		return sole
	}
	return nil
}

// PutRecord upserts on (url, user). An empty-user record is REPLACED by a record for the same url that names a
// user (the lift filling in), never duplicated beside it. Returns whether an existing record was replaced.
func (s *State) PutRecord(r CloudRecord) (replaced bool) {
	for i := range s.Clouds {
		c := &s.Clouds[i]
		if !sameURL(c.URL, r.URL) {
			continue
		}
		if c.User == r.User || c.User == "" || r.User == "" {
			if r.User == "" {
				r.User = c.User // never demote a named record to nameless
			}
			if r.Token == "" {
				r.Token, r.Expires = c.Token, c.Expires // a re-register keeps the token it already holds
			}
			if r.RouterID == "" {
				r.RouterID = c.RouterID
			}
			s.Clouds[i] = r
			return true
		}
	}
	s.Clouds = append(s.Clouds, r)
	return false
}

// RemoveRecord deletes the (url, user) record and clears every folder ref that resolved to it.
func (s *State) RemoveRecord(url, user string) (removed bool, refsCleared int) {
	rec := s.RecordFor(url, user)
	if rec == nil {
		return false, 0
	}
	keepUser := rec.User
	out := s.Clouds[:0]
	for _, c := range s.Clouds {
		if sameURL(c.URL, url) && c.User == keepUser {
			removed = true
			continue
		}
		out = append(out, c)
	}
	s.Clouds = out
	for i := range s.Folders {
		f := &s.Folders[i]
		if f.Cloud != nil && sameURL(f.Cloud.URL, url) && (f.Cloud.User == keepUser || f.Cloud.User == "") {
			f.Cloud = nil
			refsCleared++
		}
	}
	return removed, refsCleared
}

// FoldersUsing counts the TEST-hat folders whose ref resolves to the (url, user) record — the wire's
// test_folder_count for that record.
func (s *State) FoldersUsing(url, user string) int {
	rec := s.RecordFor(url, user)
	if rec == nil {
		return 0
	}
	n := 0
	for i := range s.Folders {
		f := &s.Folders[i]
		if f.Cloud == nil || !f.Hat.CanAccessScenarios() {
			continue
		}
		if s.resolveRef(f.Cloud) == rec {
			n++
		}
	}
	return n
}

// Holds is the wire's holder_count for one record: 1 when the record exists and carries a token, else 0.
func (s *State) Holds(url, user string) int {
	if rec := s.RecordFor(url, user); rec != nil && strings.TrimSpace(rec.Token) != "" {
		return 1
	}
	return 0
}

func (s *State) resolveRef(ref *CloudRef) *CloudRecord {
	if ref == nil {
		return nil
	}
	return s.RecordFor(ref.URL, ref.User)
}

// Credential resolves a folder's ref against the state and returns the author-plane target. A folder without a
// ref, or whose ref names no record, refuses with the wording the router has always used.
func (f *Folder) Credential(st *State) (Target, error) {
	if f.Cloud == nil {
		return Target{}, fmt.Errorf("router: no control plane is configured for this folder, so the author tools have nowhere to go")
	}
	rec := st.resolveRef(f.Cloud)
	if rec == nil {
		return Target{}, fmt.Errorf("router: this folder refers to a control-plane record (%s, user %q) this machine no longer holds — onboard again", f.Cloud.URL, f.Cloud.User)
	}
	if strings.TrimSpace(rec.Token) == "" {
		return Target{}, fmt.Errorf("router: this machine holds no author token for %s yet — onboarding mints it at step 8b", rec.URL)
	}
	return Target{URL: rec.URL, Token: rec.Token, Plane: "author"}, nil
}

// SetRecordToken stores a token on the (url, user) record and saves. Refuses an empty token: an empty record reads
// back as "holds nothing", the state this write exists to prevent.
func SetRecordToken(dir, url, user, token string, expires time.Time) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("router: refusing to store an EMPTY author token on this machine's record for %s", url)
	}
	return UpdateState(dir, func(st *State) error {
		rec := st.RecordFor(url, user)
		if rec == nil {
			return fmt.Errorf("router: no record for %s (user %q) on this machine — `router register` (onboarding step 8a) creates it", url, user)
		}
		if rec.Token == token {
			return ErrNoChange // idempotent, and no needless write of a credential
		}
		rec.Token, rec.Expires = token, expires
		return nil
	})
}

// ErrNoChange is what an UpdateState mutator returns to say "nothing to write" — the state is left as it is
// and UpdateState reports success.
var ErrNoChange = errors.New("router: no change")

// stateMu serializes every in-process read-modify-write of state.json. V27-009 runs ONE heartbeat loop PER
// RECORD, and two loops that both do load → mutate → save at the same instant lose one of the writes
// (measured by the 0.3.29 adversary gate: 40 of 40 rounds lost a rotated token, and the loop then
// ACKNOWLEDGED the rotation it had not stored — which revokes the old token while the machine holds
// nothing). A process-wide lock is the whole fix: the daemon is the only long-lived writer of its state.
var stateMu sync.Mutex

// UpdateState loads the state, applies fn and saves it, as one critical section. fn returning ErrNoChange
// skips the save; any other error aborts without writing.
func UpdateState(dir string, fn func(*State) error) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	st, err := LoadState(dir)
	if err != nil {
		return fmt.Errorf("router state: %w", err)
	}
	if err := fn(&st); err != nil {
		if errors.Is(err, ErrNoChange) {
			return nil
		}
		return err
	}
	return SaveState(dir, st)
}

// ReadRecordToken returns the (url, user) record's token, "" when the record or its token is absent. The VR-P7
// check stands: a PRODUCT folder carrying a ref is corrupt state and is refused before anything is read.
func ReadRecordToken(dir, url, user string) (string, error) {
	st, err := LoadState(dir)
	if err != nil {
		return "", fmt.Errorf("router state: %w", err)
	}
	for i := range st.Folders {
		if st.Folders[i].Cloud != nil && !st.Folders[i].Hat.CanAccessScenarios() {
			return "", fmt.Errorf("router state is corrupt: folder %q carries the PRODUCT hat and a cloud reference, which VR-P7 forbids — refusing to read it. Remove the reference or re-onboard that folder", st.Folders[i].Path)
		}
	}
	rec := st.RecordFor(url, user)
	if rec == nil {
		return "", nil
	}
	return strings.TrimSpace(rec.Token), nil
}

// liftLegacyClouds converts a 0.3.28 state — folder entries carrying {URL, Token} — into one CloudRecord per
// distinct URL (the token of the LAST folder that carried it; V29-02 §0.14 T1-a) plus a CloudRef per folder.
// Idempotent: a state with no legacy entries is returned unchanged with lifted == 0.
func liftLegacyClouds(st *State, legacy map[int]legacyCloud, routerID string) (lifted int) {
	if len(legacy) == 0 {
		return 0
	}
	for idx := 0; idx < len(st.Folders); idx++ {
		lc, ok := legacy[idx]
		if !ok {
			continue
		}
		if rec := st.RecordFor(lc.URL, ""); rec != nil {
			if lc.Token != "" {
				rec.Token = lc.Token // last folder wins
			}
		} else {
			st.Clouds = append(st.Clouds, CloudRecord{URL: lc.URL, Token: lc.Token, RouterID: routerID})
			lifted++
		}
		st.Folders[idx].Cloud = &CloudRef{URL: lc.URL}
	}
	return lifted
}

type legacyCloud struct {
	URL   string
	Token string
}

// HasRecord reports whether this machine holds a record for (url, user) — the check `cloud-mint-token`
// makes BEFORE asking the control plane for a token, so a machine with nowhere to put one never causes a
// live token to be minted (VR-B7). A nameless user resolves only when exactly one record has the URL.
func HasRecord(dir, url, user string) bool {
	st, err := LoadState(dir)
	if err != nil {
		return false
	}
	return st.RecordFor(url, user) != nil
}
