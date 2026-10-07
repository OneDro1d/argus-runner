package k8supgrade

import (
	"fmt"
	"sort"
)

// alignToLive returns a copy of the prepared desired document in which every value Diff called equal is
// carried exactly as the live object holds it, so a patch built from it cannot change what Diff called
// unchanged. Two things are carried:
//
//   - the ORDER of a keyed list (the lists listKey knows: env, containers, volumes, volumeMounts, ports,
//     imagePullSecrets, ...). Diff matches such a list by key, so the same entries in another order are
//     "unchanged"; but the API server takes the PATCH's order for the merged list, and a different order in a
//     pod template is a different template hash, which rolls the Deployment. Entries the live list holds keep
//     their live order; an entry live lacks (a real addition) is inserted after the desired entry that
//     precedes it.
//   - a scalar Diff called equal though the strings differ (a quantity: "500m" against "0.5"), as the live
//     string. A live value that is absent is never carried (nil would delete the key).
//
// The walk mirrors `walk`, path labels included, because listKey decides on the path.
func alignToLive(path string, d, l any) any {
	switch dv := d.(type) {
	case map[string]any:
		lm, ok := l.(map[string]any)
		if !ok {
			return deepCopy(d)
		}
		out := make(map[string]any, len(dv))
		for k, x := range dv {
			if lv, present := lm[k]; present {
				out[k] = alignToLive(join(path, k), x, lv)
			} else {
				out[k] = deepCopy(x)
			}
		}
		return out
	case []any:
		ll, ok := l.([]any)
		if !ok {
			return deepCopy(d)
		}
		key := listKey(path, dv)
		if key == "" {
			// No merge key: the server replaces the list whole and Diff compared it by position, so the
			// entries are already in live order; align each one in place.
			out := make([]any, len(dv))
			for i, x := range dv {
				if len(dv) == len(ll) {
					out[i] = alignToLive(fmt.Sprintf("%s[%d]", path, i), x, ll[i])
				} else {
					out[i] = deepCopy(x)
				}
			}
			return out
		}
		return alignKeyed(path, key, dv, ll)
	default:
		if l != nil && scalarEqual(path, d, l) {
			return l
		}
		return d
	}
}

// slot is one entry of an aligned keyed list, remembering its index in the desired list.
type slot struct {
	id int
	e  any
}

func alignKeyed(path, key string, dv, ll []any) []any {
	liveIndex := func(dm map[string]any) int {
		for i, le := range ll {
			if lm, ok := le.(map[string]any); ok && lm[key] != nil && keyStr(lm[key]) == keyStr(dm[key]) {
				return i
			}
		}
		return -1
	}
	var res []slot
	liveAt := map[int]int{} // desired index -> live index
	for i, de := range dv {
		dm := de.(map[string]any)
		if li := liveIndex(dm); li >= 0 {
			liveAt[i] = li
			label := fmt.Sprintf("%s[%s]", path, keyStr(dm[key]))
			res = append(res, slot{i, alignToLive(label, de, ll[li])})
		}
	}
	sort.SliceStable(res, func(a, b int) bool { return liveAt[res[a].id] < liveAt[res[b].id] })
	for i, de := range dv {
		if _, isMatched := liveAt[i]; isMatched {
			continue
		}
		pos := 0 // an addition with no desired predecessor goes first
		for p, s := range res {
			if i > 0 && s.id == i-1 {
				pos = p + 1
				break
			}
		}
		res = append(res, slot{})
		copy(res[pos+1:], res[pos:])
		res[pos] = slot{i, deepCopy(de)}
	}
	out := make([]any, len(res))
	for i, s := range res {
		out[i] = s.e
	}
	return out
}
