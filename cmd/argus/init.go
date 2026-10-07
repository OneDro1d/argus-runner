package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// containerized reports whether we are running inside a container. Used only to decide whether the
// bind-mount check below is meaningful: on a host binary the target is always real.
func containerized() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd") || strings.Contains(s, "kubepods")
}

// onBindMount reports whether abs sits under a mount point other than "/" — i.e. whether writes there
// reach the HOST. Returns true when it cannot tell, so this never cries wolf.
func onBindMount(abs string) bool {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return true
	}
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Fields(ln)
		if len(f) < 5 {
			continue
		}
		mp := f[4]
		if mp == "/" || mp == "" {
			continue
		}
		if abs == mp || strings.HasPrefix(abs, strings.TrimSuffix(mp, "/")+"/") {
			return true
		}
	}
	return false
}

// bundleDir is where the onedroid-testing-suite image bakes the onboarding kit
// (onboarding/ + deploy/compose/ + skills/) so the image is self-contained. `argus init
// <dir>` extracts it — a colleague with ONLY the image needs no repo. Overridable via
// ARGUS_BUNDLE_DIR (tests / non-default builds).
const bundleDir = "/opt/onedroid"

// cmdInit extracts the baked onboarding bundle into <dir> (default "."). PRE-AUTH: it needs no
// tokens — the colleague runs it before any token pair exists.
const initUsage = "usage: argus init [dir]\n" +
	"  Extracts the baked onboarding bundle (onboarding/ + deploy/compose/ + skills/) into dir\n" +
	"  (default \".\"). PRE-AUTH: no token pair exists yet when this normally runs."

func cmdInit(args []string) int {
	// F-CLI-HELP-1: `dst` is a bare positional, so `argus init --help` used to be read as "extract
	// into a directory literally named --help" and fail with a confusing "no onboarding bundle"
	// error instead of ever answering the question asked.
	if len(args) > 0 && isHelpToken(args[0]) {
		fmt.Println(initUsage)
		return exitOK
	}
	dst := "."
	if len(args) > 0 && args[0] != "" {
		dst = args[0]
	}
	src := bundleDir
	if v := os.Getenv("ARGUS_BUNDLE_DIR"); v != "" {
		src = v
	}
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return emitErr(exitErr, "no onboarding bundle at %s — `init` requires the onedroid-testing-suite image", src)
	}
	replaced, err := copyTree(src, dst)
	if err != nil {
		return emitErr(exitErr, "init: %v", err)
	}
	// VR4-K1: say what was overwritten. `init` is a legitimate destructive act — extracting the kit is
	// the point — so it does not refuse. What it may not do is destroy the operator's own edits in
	// silence, which is how this round's kit-side workaround was discarded by the documented happy
	// path. Naming the files turns an invisible loss into a decision the operator can act on.
	if len(replaced) > 0 {
		fmt.Fprintf(os.Stderr, "NOTE: init REPLACED %d file(s) that differed from the image's copy:\n", len(replaced))
		for _, r := range replaced {
			fmt.Fprintf(os.Stderr, "        %s\n", r)
		}
		fmt.Fprintf(os.Stderr, "      Local edits to those files are gone. If you had patched this kit, re-apply the\n"+
			"      patch — an onboard run now uses the image's version.\n")
	}
	// Did the extraction actually reach the HOST? If `init` runs in a container and the target is NOT
	// under a bind mount, every file just written lives on the container's own writable layer and is
	// DESTROYED when it exits — which, with the documented `docker run --rm`, is immediately. The old
	// behaviour printed {"initialized": true} and the operator found an empty directory.
	//
	// Measured on Windows + Git Bash, 2026-08-05: `-v /c/tmp:/out` silently mounts NOTHING (MSYS
	// rewrites the source path), while `-v C:/tmp:/out` mounts correctly. Both reported success.
	// Reporting a check you did not perform as a pass is the failure mode this whole sweep keeps
	// finding, so say it plainly instead.
	//
	// This WARNS rather than refuses: extracting inside a container and `docker cp`-ing the result out
	// is a legitimate workflow, and refusing would break it.
	persisted := true
	abs, aerr := filepath.Abs(dst)
	if aerr == nil && containerized() && !onBindMount(abs) {
		persisted = false
		fmt.Fprintf(os.Stderr, "\n  WARNING: %s is NOT on a bind mount.\n", abs)
		fmt.Fprintf(os.Stderr, "  The kit was written INSIDE this container and will be lost when it exits\n")
		fmt.Fprintf(os.Stderr, "  (immediately, if you passed --rm). Nothing will appear on your machine.\n\n")
		fmt.Fprintf(os.Stderr, "  Mount a host directory onto the target's parent, e.g.\n")
		fmt.Fprintf(os.Stderr, "    docker run --rm -v C:/tmp:/out <image> init /out/my-kit\n")
		fmt.Fprintf(os.Stderr, "  On Windows + Git Bash use the WINDOWS form (C:/tmp), not /c/tmp — the POSIX\n")
		fmt.Fprintf(os.Stderr, "  form is rewritten before Docker sees it and mounts nothing.\n\n")
	}
	out := map[string]any{
		"initialized": true, "bundle": src, "into": dst,
		// false = written to the container's own layer; it will NOT survive the container.
		"persisted": persisted,
		"next": "create your product + test folders, write argus-config.yaml into the product folder, then run: bash " +
			filepath.ToSlash(filepath.Join(dst, "onboarding", "onboard.sh")) + " --product-dir <P> --scenarios-dir <T> --image <this-image>",
	}
	if !persisted {
		out["warning"] = "target is not a bind mount — these files will be discarded when the container exits"
	}
	emit(out)
	return exitOK
}

// copyTree recursively copies src/* into dst, preserving the relative layout. Files are written
// 0o755 so onboard.sh stays executable after extraction.
//
// It returns the relative paths whose CONTENT it replaced — files that existed in dst and differed.
// VR4-K1 (V18-005): `init` used to overwrite a locally-patched kit with no warning and no diff. That
// matters more than tidiness here, because this round's standing rule is "fix locally, batch the
// build": kit-side fixes live only on disk until a publish, and the samples doc runs `argus init`
// as step 1. The documented happy path therefore discarded the round's own workaround, and the
// onboard that followed ran on reverted code while looking green.
//
// A file that is byte-identical, or that did not exist, is NOT reported. Nothing was lost in either
// case, and a warning that cries wolf is one operators learn to scroll past.
func copyTree(src, dst string) ([]string, error) {
	var replaced []string
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		// Read BEFORE writing — afterwards there is nothing left to compare against, which is the
		// whole reason the old behaviour could not tell the operator what it had done.
		if old, oerr := os.ReadFile(target); oerr == nil && !bytes.Equal(old, b) {
			replaced = append(replaced, filepath.ToSlash(rel))
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o755)
	})
	return replaced, err
}
