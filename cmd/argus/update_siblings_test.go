package main

import (
	"testing"

	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/router"
	"github.com/OneDro1d/argus-runner/internal/updatecmd"
)

// A sibling instance that shares a skills folder but never wrote an installed manifest (onboarded
// before recordInstalledManifest existed, or by the predecessor tool) must still show up as a
// sibling — present and of unknown version — so the shared-folder rule holds the folder back. Before
// this fix, the sibling list came only from files under installed/, so a manifest-less sibling was
// never even considered: "no manifest" read as "no sibling" instead of "unknown".
func TestLocalSiblings_AManifestlessSiblingWiredToTheSameFolderIsSeenAsUnknown(t *testing.T) {
	state := t.TempDir()
	shared := "/agents/shared-folder"

	// The router already knows BOTH instances are wired to the shared folder — this is independent
	// of whether either instance ever wrote installed/<id>.json.
	if err := router.SaveState(state, router.State{Folders: []router.Folder{
		{
			Path: shared,
			Hat:  role.Product,
			Upstreams: map[string]router.Upstream{
				"i1": {URL: "http://i1:9999"},
				"i2": {URL: "http://i2:9999"},
			},
		},
	}}); err != nil {
		t.Fatal(err)
	}

	// i2 never onboarded through recordInstalledManifest — no installed/i2.json exists at all.
	ourFolders := []updatecmd.Folder{{Path: shared}}
	sibs := planSiblings(updateArgs{RouterState: state, InstanceID: "i1"}, ourFolders)

	var got *updatecmd.Sibling
	for i := range sibs {
		if sibs[i].InstanceID == "i2" {
			got = &sibs[i]
		}
	}
	if got == nil {
		t.Fatalf("i2 is wired to the shared folder but has no manifest, and was not seen as a sibling at all: %+v", sibs)
	}
	if got.InstalledVersion != "" {
		t.Errorf("a manifest-less sibling must read as version UNKNOWN, got %q", got.InstalledVersion)
	}
	found := false
	for _, p := range got.SkillsHostPaths {
		if p == shared {
			found = true
		}
	}
	if !found {
		t.Errorf("i2's shared path must include %q (what the router already knows it shares), got %v", shared, got.SkillsHostPaths)
	}
}

// A sibling with NO router wiring to any of our folders, and no manifest, is not a sibling at all —
// the fix must not manufacture blockers for instances that share nothing.
func TestLocalSiblings_AnUnrelatedManifestlessInstanceIsNotASibling(t *testing.T) {
	state := t.TempDir()
	if err := router.SaveState(state, router.State{Folders: []router.Folder{
		{
			Path:      "/agents/other-folder",
			Hat:       role.Product,
			Upstreams: map[string]router.Upstream{"i3": {URL: "http://i3:9999"}},
		},
	}}); err != nil {
		t.Fatal(err)
	}

	ourFolders := []updatecmd.Folder{{Path: "/agents/shared-folder"}}
	sibs := planSiblings(updateArgs{RouterState: state, InstanceID: "i1"}, ourFolders)

	for _, s := range sibs {
		if s.InstanceID == "i3" {
			t.Fatalf("i3 shares nothing with i1's folders and must not appear as a sibling: %+v", sibs)
		}
	}
}
