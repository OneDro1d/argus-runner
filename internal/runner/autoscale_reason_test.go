package runner

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// The scale log names WHY a replica count was chosen. It used to pick its label by comparing the count
// with the active floor, so once the #215 cap made both floors 1, a parked executor logged
// "scaled to 1 replicas (active/min-3)" — seen live on argus-inst-shop-uni-arb and
// argus-inst-shop-dev, 2026-09-24 16:05Z, with no run in flight. The label must come from the state.

type logRec struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRec) log(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logRec) scaleLines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, s := range l.lines {
		if strings.Contains(s, "scaled to") {
			out = append(out, s)
		}
	}
	return out
}

// equalFloors is an autoscaler whose active and idle floors are both 1, the #215 cap's shape.
func equalFloors(t *testing.T) (*Autoscaler, *logRec) {
	t.Helper()
	fd := &fakeDeploy{}
	lr := &logRec{}
	a := fd.replica(defaultIdleCooldown)
	a.ActiveReplicas, a.IdleReplicas = 1, 1
	a.Log = lr.log
	return a, lr
}

func TestAutoscaler_ScaleLogNamesIdleWhenFloorsAreEqual(t *testing.T) {
	a, lr := equalFloors(t)
	a.reconcile(context.Background()) // never active: the idle floor
	got := lr.scaleLines()
	if len(got) != 1 {
		t.Fatalf("want one scale line, got %q", got)
	}
	if !strings.Contains(got[0], "idle-parked") || strings.Contains(got[0], "active") {
		t.Errorf("a never-active executor must log idle-parked, got %q", got[0])
	}
}

func TestAutoscaler_ScaleLogNamesTheRealActiveFloor(t *testing.T) {
	a, lr := equalFloors(t)
	a.MarkActive(context.Background())
	got := lr.scaleLines()
	if len(got) != 1 {
		t.Fatalf("want one scale line, got %q", got)
	}
	if !strings.Contains(got[0], "active, floor 1") || strings.Contains(got[0], "min-3") {
		t.Errorf("an active executor capped at 1 must say its real floor, got %q", got[0])
	}
}

// A replica that never ran anything itself, but sees another replica's lease, scales to the active
// floor because of that run, and must say so.
func TestAutoscaler_ScaleLogNamesALeaseHeldRunActive(t *testing.T) {
	fd := &fakeDeploy{lease: time.Now().Add(time.Hour)}
	lr := &logRec{}
	a := fd.replica(defaultIdleCooldown)
	a.Log = lr.log
	a.reconcile(context.Background())
	got := lr.scaleLines()
	if len(got) != 1 {
		t.Fatalf("want one scale line, got %q", got)
	}
	if !strings.Contains(got[0], "active, floor 3") {
		t.Errorf("a replica held at the active floor by another's lease must log active, got %q", got[0])
	}
}

func TestScaleReason(t *testing.T) {
	cases := []struct {
		active  bool
		activeN int
		want    string
	}{
		{true, 3, "active, floor 3"},
		{true, 1, "active, floor 1"},
		{false, 3, "idle-parked"},
		{false, 1, "idle-parked"},
	}
	for _, c := range cases {
		if got := scaleReason(c.active, c.activeN); got != c.want {
			t.Errorf("scaleReason(%v, %d) = %q, want %q", c.active, c.activeN, got, c.want)
		}
	}
}
