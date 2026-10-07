package router

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// #607: the port written is the one the LISTENER holds, and the file is whole JSON
// naming the writer's pid — the two facts native onboarding checks before it believes a /ready answer.
func TestWriteBoundFile_NamesTheListenersPortAndPid(t *testing.T) {
	// ":0" asks the kernel for ANY free loopback port: the answer can only come from the listener itself,
	// which is the point (a port read from anywhere else is a claim, not a bind).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port, err := ListenerPort(ln)
	if err != nil {
		t.Fatalf("ListenerPort: %v", err)
	}
	if want := ln.Addr().(*net.TCPAddr).Port; port != want || port == 0 {
		t.Fatalf("ListenerPort = %d, want the bound %d", port, want)
	}

	path := filepath.Join(t.TempDir(), "state", "native-router.bound")
	if err := WriteBoundFile(path, 4242, port); err != nil {
		t.Fatalf("WriteBoundFile: %v", err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got Bound
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("the file is not whole JSON (%v): %q", err, blob)
	}
	if got.PID != 4242 || got.Port != port {
		t.Fatalf("file says %+v, want pid 4242 port %d", got, port)
	}
	// Rewritten in place by a later start: the reader sees the new content, and no temp file is left behind.
	if err := WriteBoundFile(path, 4343, port); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	blob, _ = os.ReadFile(path)
	if err := json.Unmarshal(blob, &got); err != nil || got.PID != 4343 {
		t.Fatalf("after a rewrite the file says %q (err %v), want pid 4343", blob, err)
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	if len(ents) != 1 {
		t.Fatalf("the directory holds %d entries after two writes, want only the file (a temp file leaked)", len(ents))
	}
}

func TestWriteBoundFile_RefusesAnEmptyPath(t *testing.T) {
	if err := WriteBoundFile("", 1, 9765); err == nil {
		t.Fatal("an empty path must be refused, not written to the working directory")
	}
}
