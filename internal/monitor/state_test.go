package monitor

import (
	"container/ring"
	"testing"
	"time"
)

func TestGetDiffHistorySkipsZeroTimestamp(t *testing.T) {
	t.Parallel()

	ms := NewMonitorState()
	ms.DiffHistory = ring.New(4)

	now := time.Now()
	r := ms.DiffHistory
	r.Value = DiffRecord{Timestamp: time.Time{}, Percentage: 99}
	r = r.Next()
	r.Value = DiffRecord{Timestamp: now.Add(-2 * time.Hour), Percentage: 1.0}
	r = r.Next()
	r.Value = DiffRecord{Timestamp: now.Add(-2 * time.Minute), Percentage: 2.0}

	recent := ms.GetDiffHistory(10*time.Minute, false)
	if len(recent) != 1 {
		t.Fatalf("expected 1 recent record, got %d", len(recent))
	}
	if recent[0].Timestamp.IsZero() {
		t.Fatalf("recent record must not include zero timestamp")
	}
	if recent[0].Percentage != 2.0 {
		t.Fatalf("unexpected recent percentage: %.2f", recent[0].Percentage)
	}

	all := ms.GetDiffHistory(0, false)
	if len(all) != 2 {
		t.Fatalf("expected 2 non-zero records, got %d", len(all))
	}
	for _, rec := range all {
		if rec.Timestamp.IsZero() {
			t.Fatalf("all records must not include zero timestamp")
		}
	}
}

func TestShouldRecordHistoryThinsSustainedZeroDiff(t *testing.T) {
	t.Parallel()

	ms := NewMonitorState()
	defer ms.StopHeatmapWorker()
	base := time.Now()
	record := func(offset time.Duration, pct float64) bool {
		return ms.shouldRecordHistoryLocked(&MonitorData{Timestamp: base.Add(offset), DiffPercentage: pct})
	}

	if !record(0, 0) {
		t.Fatalf("first zero-diff record must be stored")
	}
	if record(10*time.Second, 0) {
		t.Fatalf("zero-diff record within the interval must be skipped")
	}
	if !record(zeroDiffHistoryInterval, 0) {
		t.Fatalf("zero-diff record must be stored once the interval elapsed")
	}
	if !record(zeroDiffHistoryInterval+time.Second, 1.5) {
		t.Fatalf("non-zero record must always be stored")
	}
	if !record(zeroDiffHistoryInterval+2*time.Second, 2.0) {
		t.Fatalf("consecutive non-zero records must always be stored")
	}
	if !record(zeroDiffHistoryInterval+3*time.Second, 0) {
		t.Fatalf("return to zero must be stored immediately")
	}
}
