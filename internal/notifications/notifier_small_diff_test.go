package notifications

import (
	"testing"
	"time"
)

func TestTrackZeroDiffForSmallDiffReset(t *testing.T) {
	t.Parallel()

	newNotifier := func() *Notifier {
		return &Notifier{
			states: map[string]*NotificationState{
				"guild-1": {SmallDiffMessageID: "msg-1", SmallDiffMessageChannelID: "ch-1"},
			},
		}
	}
	start := time.Now()

	n := newNotifier()
	st := n.states["guild-1"]
	n.trackZeroDiffForSmallDiffReset(true, start)
	n.trackZeroDiffForSmallDiffReset(true, start.Add(smallDiffTrackingResetAfter-time.Second))
	if st.SmallDiffMessageID != "msg-1" {
		t.Fatalf("pointer must be kept before the zero-diff window elapses")
	}
	n.trackZeroDiffForSmallDiffReset(true, start.Add(smallDiffTrackingResetAfter))
	if st.SmallDiffMessageID != "" {
		t.Fatalf("pointer should be cleared after sustained zero diff, got %q", st.SmallDiffMessageID)
	}

	// Reset happens once per zero-diff period.
	st.SmallDiffMessageID = "msg-2"
	n.trackZeroDiffForSmallDiffReset(true, start.Add(2*smallDiffTrackingResetAfter))
	if st.SmallDiffMessageID != "msg-2" {
		t.Fatalf("pointer must not be cleared twice in the same zero-diff period")
	}

	// A non-zero diff restarts the window.
	n = newNotifier()
	st = n.states["guild-1"]
	n.trackZeroDiffForSmallDiffReset(true, start)
	n.trackZeroDiffForSmallDiffReset(false, start.Add(5*time.Minute))
	n.trackZeroDiffForSmallDiffReset(true, start.Add(6*time.Minute))
	n.trackZeroDiffForSmallDiffReset(true, start.Add(smallDiffTrackingResetAfter+time.Minute))
	if st.SmallDiffMessageID != "msg-1" {
		t.Fatalf("pointer must be kept when the zero-diff window was interrupted")
	}
}

func TestResetAllSmallDiffMessageTrackingClearsPointers(t *testing.T) {
	t.Parallel()

	nextUpdate := time.Now().Add(5 * time.Second)
	n := &Notifier{
		states: map[string]*NotificationState{
			"guild-1": {
				SmallDiffMessageID:        "msg-1",
				SmallDiffMessageChannelID: "ch-1",
				SmallDiffLastContent:      "old content",
				SmallDiffNextUpdate:       nextUpdate,
			},
		},
	}

	n.resetAllSmallDiffMessageTracking()

	st := n.states["guild-1"]
	if st.SmallDiffMessageID != "" {
		t.Fatalf("SmallDiffMessageID should be cleared, got %q", st.SmallDiffMessageID)
	}
	if st.SmallDiffMessageChannelID != "" {
		t.Fatalf("SmallDiffMessageChannelID should be cleared, got %q", st.SmallDiffMessageChannelID)
	}
	if st.SmallDiffLastContent != "" {
		t.Fatalf("SmallDiffLastContent should be cleared, got %q", st.SmallDiffLastContent)
	}
	if !st.SmallDiffNextUpdate.IsZero() {
		t.Fatalf("SmallDiffNextUpdate should be zero, got %s", st.SmallDiffNextUpdate)
	}
}
