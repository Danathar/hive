package main

import (
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func funcName(f interface{}) string {
	full := runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
	return full[strings.LastIndex(full, ".")+1:]
}

func TestDefaultBootSequenceOrder(t *testing.T) {
	seq := defaultBootSequence()
	if got := funcName(seq.config); got != "bootConfig" {
		t.Fatalf("config phase = %s", got)
	}
	if got := funcName(seq.loop); got != "runLoop" {
		t.Fatalf("loop = %s", got)
	}
	want := []string{
		"bootGitHub", "bootGovernor", "bootAdvisory", "bootAgents", "bootState",
		"bootDashboard", "bootStores", "bootCollectors", "bootKnowledge",
		"bootSupervision", "bootDashboardAPI", "bootPolicies", "bootWatchers",
		"bootProxy", "bootLaunch", "bootHeartbeat", "bootLanes",
	}
	if len(seq.phases) != len(want) {
		t.Fatalf("phase count = %d, want %d", len(seq.phases), len(want))
	}
	// step() wraps non-veto phases in a closure, so their names are not
	// recoverable through the func value; only the veto-capable bootAdvisory
	// is pinned by name. defaultBootSequenceNames pins the rest.
	if got := funcName(seq.phases[2]); got != "bootAdvisory" {
		t.Fatalf("phases[2] = %s, want bootAdvisory (the veto-capable phase)", got)
	}
	if got := defaultBootSequenceNames(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("phase order:\n got %v\nwant %v", got, want)
	}
}

func TestRunBootRunsPhasesInOrderThenLoopThenCleanup(t *testing.T) {
	var trace []string
	b := &boot{}
	b.cleanup.push(func() { trace = append(trace, "cleanup") })
	seq := bootSequence{
		config: func(*boot) bool { trace = append(trace, "config"); return true },
		phases: []bootPhase{
			func(*boot) bool { trace = append(trace, "p1"); return true },
			func(*boot) bool { trace = append(trace, "p2"); return true },
		},
		loop: func(*boot) { trace = append(trace, "loop") },
	}
	runBoot(b, seq)
	if got := strings.Join(trace, ","); got != "config,p1,p2,loop,cleanup" {
		t.Fatalf("trace = %s", got)
	}
}

func TestRunBootConfigVetoSkipsPhasesButRunsCleanup(t *testing.T) {
	var trace []string
	b := &boot{}
	b.cleanup.push(func() { trace = append(trace, "cleanup") })
	seq := bootSequence{
		config: func(*boot) bool { trace = append(trace, "config"); return false },
		phases: []bootPhase{func(*boot) bool { t.Fatal("phase must not run after veto"); return true }},
		loop:   func(*boot) { t.Fatal("loop must not run after veto") },
	}
	runBoot(b, seq)
	if got := strings.Join(trace, ","); got != "config,cleanup" {
		t.Fatalf("trace = %s", got)
	}
}

func TestRunBootPhaseVetoSkipsLaterPhasesAndLoop(t *testing.T) {
	var trace []string
	b := &boot{}
	b.cleanup.push(func() { trace = append(trace, "cleanup") })
	seq := bootSequence{
		config: func(*boot) bool { trace = append(trace, "config"); return true },
		phases: []bootPhase{
			func(*boot) bool { trace = append(trace, "p1"); return false },
			func(*boot) bool { t.Fatal("later phase must not run after a veto"); return true },
		},
		loop: func(*boot) { t.Fatal("loop must not run after a phase veto") },
	}
	runBoot(b, seq)
	if got := strings.Join(trace, ","); got != "config,p1,cleanup" {
		t.Fatalf("trace = %s", got)
	}
}
