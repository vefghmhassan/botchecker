package store

import (
	"path/filepath"
	"testing"
	"time"
)

// A process killed mid-scan leaves a row claiming to be in progress. Without
// reaping it, the dashboard reports a scan that will never finish.
func TestReapInterruptedScans(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	stale, err := st.CreateScan("manual", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	done, err := st.CreateScan("scheduled", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishScan(done, ScanCompleted, 0, nil, now.Add(-90*time.Minute)); err != nil {
		t.Fatal(err)
	}

	n, err := st.ReapInterruptedScans(now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reaped %d scans, want only the running one", n)
	}

	got, err := st.GetScan(stale)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ScanFailed {
		t.Errorf("status = %s, want %s", got.Status, ScanFailed)
	}
	if got.FinishedAt == nil {
		t.Error("the reaped scan has no finish time")
	}
	if got.Error == "" {
		t.Error("the reaped scan does not say why it failed")
	}

	// A completed scan must not be touched.
	finished, err := st.GetScan(done)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != ScanCompleted {
		t.Errorf("a completed scan was reaped: %s", finished.Status)
	}
}
