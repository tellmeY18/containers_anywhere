package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func readData(t *testing.T, state string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(state, "data.img"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func selectedIs(t *testing.T, state string, want manifest) bool {
	t.Helper()
	m, err := readManifest(filepath.Join(state, "release.json"))
	if err != nil {
		t.Fatal(err)
	}
	return bundleID(m) == bundleID(want)
}

func TestSwapRequiresIdenticalK3s(t *testing.T) {
	_, m := testBundle(t)
	n := m
	n.System = "/nix/store/other"
	if err := swapEligible(m, n); err != nil {
		t.Fatal("root-only change rejected:", err)
	}
	n.K3s = "1.35.1+k3s1"
	if err := swapEligible(m, n); err == nil {
		t.Fatal("a K3s change must use the full update transaction")
	}
	if err := swapEligible(m, m); err == nil {
		t.Fatal("swapping to the selected appliance must be rejected")
	}
}

func TestPrepareSwapSelectsCandidateAndKeepsDataAndFallbackRoot(t *testing.T) {
	state, old, next := updateFixture(t)
	before := readData(t, state)
	oldManifest, _ := readManifest(filepath.Join(old, "manifest.json"))
	nextManifest, _ := readManifest(filepath.Join(next, "manifest.json"))
	j, err := prepareSwap(state, next, old, updateHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if j.Phase != "trying" || !selectedIs(t, state, nextManifest) {
		t.Fatal("candidate was not selected behind a durable trying journal")
	}
	if readData(t, state) != before {
		t.Fatal("a root swap must not touch the data disk")
	}
	for _, dir := range []string{j.ID, j.ID + "-original"} {
		if _, err := verifyBundle(filepath.Join(state, "updates", dir, "candidate")); err != nil {
			t.Fatalf("retained root %s is not launchable: %v", dir, err)
		}
	}
	if selected, _ := readManifest(filepath.Join(state, "release.json")); retainedFor(t, state, selected) == "" {
		t.Fatal("selected candidate cannot be found for launch")
	}
	if err := rollbackSwap(state, &j, "test", updateHooks{}); err != nil {
		t.Fatal(err)
	}
	if !selectedIs(t, state, oldManifest) || readData(t, state) != before {
		t.Fatal("rollback must reselect the original root and leave the data alone")
	}
	if got, _ := readSwap(state); got.Phase != "rolled-back" {
		t.Fatalf("phase = %s", got.Phase)
	}
	if selected, _ := readManifest(filepath.Join(state, "release.json")); retainedFor(t, state, selected) == "" {
		t.Fatal("original root is not retained for fallback")
	}
}

func retainedFor(t *testing.T, state string, selected manifest) string {
	t.Helper()
	dir, err := retainedUpdateBundle(state, selected)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInterruptedSwapRollsBackOnNextStart(t *testing.T) {
	state, old, next := updateFixture(t)
	oldManifest, _ := readManifest(filepath.Join(old, "manifest.json"))
	j, err := prepareSwap(state, next, old, updateHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepareSwap(state, next, old, updateHooks{}); err == nil {
		t.Fatal("a second swap must not start while a trial is unresolved")
	}
	swapMu.Lock()
	swapSession = j.ID
	swapMu.Unlock()
	if err := recoverSwap(state, updateHooks{}); err != nil || got(t, state) != "trying" {
		t.Fatal("the live trial must not be rolled back by its own session", err)
	}
	swapMu.Lock()
	swapSession = ""
	swapMu.Unlock()
	if err := recoverSwap(state, updateHooks{}); err != nil {
		t.Fatal(err)
	}
	if got(t, state) != "rolled-back" || !selectedIs(t, state, oldManifest) {
		t.Fatal("a crashed trial must roll back to the original root")
	}
}

func got(t *testing.T, state string) string {
	t.Helper()
	j, err := readSwap(state)
	if err != nil {
		t.Fatal(err)
	}
	return j.Phase
}

func TestCrashAtEveryPublicationBoundaryNeverLeavesAnUnrecoverableSelection(t *testing.T) {
	for _, boundary := range []string{"swap-retained", "swap-trying", "swap-selected"} {
		t.Run(boundary, func(t *testing.T) {
			state, old, next := updateFixture(t)
			oldManifest, _ := readManifest(filepath.Join(old, "manifest.json"))
			before := readData(t, state)
			h := updateHooks{boundary: func(name string) error {
				if name == boundary {
					return os.ErrDeadlineExceeded
				}
				return nil
			}}
			if _, err := prepareSwap(state, next, old, h); err == nil {
				t.Fatal("expected the injected crash")
			}
			if err := recoverSwap(state, updateHooks{}); err != nil {
				t.Fatal(err)
			}
			if !selectedIs(t, state, oldManifest) {
				t.Fatal("the original root must be selected after recovery")
			}
			if retainedFor(t, state, oldManifest) == "" && boundary != "swap-retained" {
				// Before the trying journal exists nothing was selected, so the
				// original is found in its own location, not the retained copy.
				t.Fatal("original root cannot be launched after recovery")
			}
			if readData(t, state) != before {
				t.Fatal("data changed")
			}
		})
	}
}

func swapFixture(t *testing.T) (string, swapJournal, manifest) {
	t.Helper()
	state, old, next := updateFixture(t)
	oldManifest, _ := readManifest(filepath.Join(old, "manifest.json"))
	j, err := prepareSwap(state, next, old, updateHooks{})
	if err != nil {
		t.Fatal(err)
	}
	return state, j, oldManifest
}

func TestSwapSessionCommitsAHealthyCandidate(t *testing.T) {
	state, j, _ := swapFixture(t)
	stop := make(chan struct{})
	calls := 0
	go func() {
		for got(t, state) != "committed" {
			time.Sleep(5 * time.Millisecond)
		}
		close(stop)
	}()
	err := runSwapSession(state, &j, swapHooks{
		serve: func() error { calls++; <-stop; return nil }, abort: func() { t.Error("healthy trial was aborted") },
		healthy: func() bool { return true }, timeout: 5 * time.Second, poll: 5 * time.Millisecond,
	})
	if err != nil || calls != 1 || got(t, state) != "committed" {
		t.Fatalf("err=%v calls=%d phase=%s", err, calls, got(t, state))
	}
}

func TestSwapSessionFallsBackWhenTheCandidateSessionFails(t *testing.T) {
	state, j, old := swapFixture(t)
	before := readData(t, state)
	calls := 0
	err := runSwapSession(state, &j, swapHooks{
		serve: func() error {
			calls++
			if calls == 1 {
				return os.ErrInvalid
			}
			if !selectedIs(t, state, old) {
				t.Error("fallback session must run the original root")
			}
			return nil
		},
		abort: func() {}, healthy: func() bool { return false }, timeout: 5 * time.Second, poll: time.Hour,
	})
	if err != nil || calls != 2 || got(t, state) != "rolled-back" || readData(t, state) != before {
		t.Fatalf("err=%v calls=%d phase=%s", err, calls, got(t, state))
	}
}

func TestSwapSessionFallsBackWhenTheCandidateNeverBecomesHealthy(t *testing.T) {
	state, j, old := swapFixture(t)
	stop := make(chan struct{})
	var once sync.Once
	aborted, calls := 0, 0
	err := runSwapSession(state, &j, swapHooks{
		serve: func() error {
			calls++
			if calls == 1 {
				<-stop
			}
			return nil
		},
		abort:   func() { aborted++; once.Do(func() { close(stop) }) },
		healthy: func() bool { return false }, timeout: 50 * time.Millisecond, poll: 5 * time.Millisecond,
	})
	if err != nil || aborted != 1 || calls != 2 || got(t, state) != "rolled-back" || !selectedIs(t, state, old) {
		t.Fatalf("err=%v aborted=%d calls=%d phase=%s", err, aborted, calls, got(t, state))
	}
}

func TestQuittingDuringTheTrialLeavesItToBeRolledBackNextStart(t *testing.T) {
	state, j, old := swapFixture(t)
	err := runSwapSession(state, &j, swapHooks{
		serve: func() error { return nil }, abort: func() {}, healthy: func() bool { return false },
		timeout: 5 * time.Second, poll: time.Hour,
	})
	if err != nil || got(t, state) != "trying" {
		t.Fatalf("err=%v phase=%s", err, got(t, state))
	}
	if err := recoverSwap(state, updateHooks{}); err != nil || !selectedIs(t, state, old) {
		t.Fatal("the next start must roll back", err)
	}
}
