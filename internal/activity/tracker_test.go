package activity

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
	"time"
)

func TestCloneUserActivityDeepCopy(t *testing.T) {
	src := &UserActivity{
		ID:                  "1",
		Name:                "user",
		DailyVandalCounts:   map[string]int{"2026-02-15": 3},
		DailyRestoredCounts: map[string]int{"2026-02-15": 1},
		DailyActivityScores: map[string]int{"2026-02-15": -2},
		LastPixel:           &PixelRef{X: 10, Y: 20},
	}

	cloned := cloneUserActivity(src)

	src.DailyVandalCounts["2026-02-15"] = 999
	src.DailyRestoredCounts["2026-02-15"] = 999
	src.DailyActivityScores["2026-02-15"] = 999
	src.LastPixel.X = 777

	if got := cloned.DailyVandalCounts["2026-02-15"]; got != 3 {
		t.Fatalf("DailyVandalCounts not deep-copied, got=%d want=3", got)
	}
	if got := cloned.DailyRestoredCounts["2026-02-15"]; got != 1 {
		t.Fatalf("DailyRestoredCounts not deep-copied, got=%d want=1", got)
	}
	if got := cloned.DailyActivityScores["2026-02-15"]; got != -2 {
		t.Fatalf("DailyActivityScores not deep-copied, got=%d want=-2", got)
	}
	if cloned.LastPixel == nil {
		t.Fatal("LastPixel should not be nil")
	}
	if got := cloned.LastPixel.X; got != 10 {
		t.Fatalf("LastPixel not deep-copied, got=%d want=10", got)
	}
}

func TestGetCurrentDiffPainterCounts(t *testing.T) {
	t.Parallel()

	tracker := NewTracker(Config{
		TopLeftTileX:  0,
		TopLeftTileY:  0,
		TopLeftPixelX: 0,
		TopLeftPixelY: 0,
		Width:         2,
		Height:        2,
	}, nil, "")

	tracker.vandalState.PixelToPainter = map[string]string{
		pixelKey(0, 0): "1001",
		pixelKey(1, 0): "1001",
		pixelKey(2, 0): "1002",
	}
	tracker.activity["1001"] = &UserActivity{ID: "1001", Name: "alice"}
	tracker.activity["1002"] = &UserActivity{ID: "1002", Name: "bob"}

	all := tracker.GetCurrentDiffPainterCounts(0)
	if len(all) != 2 {
		t.Fatalf("expected 2 users, got %d", len(all))
	}
	if all[0].UserID != "1001" || all[0].Pixels != 2 {
		t.Fatalf("unexpected top entry: %+v", all[0])
	}
	if all[1].UserID != "1002" || all[1].Pixels != 1 {
		t.Fatalf("unexpected second entry: %+v", all[1])
	}

	limited := tracker.GetCurrentDiffPainterCounts(1)
	if len(limited) != 1 {
		t.Fatalf("expected 1 entry with limit, got %d", len(limited))
	}
	if limited[0].UserID != "1001" || limited[0].Name != "alice" {
		t.Fatalf("unexpected limited entry: %+v", limited[0])
	}
}

func TestInferenceLifecycle(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 2, 20, 12, 0, 0, 0, time.UTC)
	var state inferenceState

	if armed := armInference(&state, 1, now); armed {
		t.Fatal("inference should not arm for single pixel")
	}
	if state.Active {
		t.Fatal("state should remain inactive")
	}

	if armed := armInference(&state, 8, now); !armed {
		t.Fatal("expected inference to be armed")
	}
	if !state.Active || state.RemainingPixels != 8 {
		t.Fatalf("unexpected armed state: %+v", state)
	}

	active, effective, credit := beginInference(&state, "1001", now.Add(2*time.Second))
	if !active {
		t.Fatal("inference should be active")
	}
	if effective != "1001" || credit != 8 {
		t.Fatalf("unexpected begin result effective=%s credit=%d", effective, credit)
	}

	consumeInference(&state)
	if state.RemainingPixels != 7 {
		t.Fatalf("remaining should decrement to 7, got %d", state.RemainingPixels)
	}

	// Subsequent detections are still attributed to the first claimed painter.
	active, effective, credit = beginInference(&state, "2002", now.Add(3*time.Second))
	if !active || effective != "1001" || credit != 0 {
		t.Fatalf("unexpected aliasing result active=%v effective=%s credit=%d", active, effective, credit)
	}

	state.ExpiresAt = now.Add(-time.Second)
	active, _, _ = beginInference(&state, "1001", now)
	if active {
		t.Fatal("expired inference should not stay active")
	}
	if state.Active {
		t.Fatal("state should be reset after expiration")
	}
}

func TestRecordRecentEventsCapped(t *testing.T) {
	t.Parallel()

	store := map[string][]time.Time{}
	now := time.Now().UTC()
	got := recordRecentEvents(store, "u1", now, newUserNotifyWindow, 999)
	if got != newUserNotifyThreshold {
		t.Fatalf("expected capped recent events count %d, got %d", newUserNotifyThreshold, got)
	}
}

func TestClearInferenceProbeOnFetchFailure(t *testing.T) {
	t.Parallel()

	vandal := inferenceState{Active: true, ProbeQueued: true}
	restore := inferenceState{Active: true, ProbeQueued: true}

	clearInferenceProbeOnFetchFailure(&vandal, &restore)

	if vandal.ProbeQueued {
		t.Fatalf("expected vandal probe flag to clear on fetch failure")
	}
	if restore.ProbeQueued {
		t.Fatalf("expected restore probe flag to clear on fetch failure")
	}
}

func TestClearInferenceProbeOnFetchFailureKeepsClaimedProbeState(t *testing.T) {
	t.Parallel()

	vandal := inferenceState{Active: true, ProbeQueued: true, ClaimedPainter: "1001"}
	restore := inferenceState{Active: true, ProbeQueued: true, ClaimedPainter: "2002"}

	clearInferenceProbeOnFetchFailure(&vandal, &restore)

	if !vandal.ProbeQueued {
		t.Fatalf("claimed vandal inference should not be altered")
	}
	if !restore.ProbeQueued {
		t.Fatalf("claimed restore inference should not be altered")
	}
}

func TestClaimCurrentDiffPixelsSkipsBaseline(t *testing.T) {
	t.Parallel()

	current := map[string]Pixel{
		pixelKey(10, 20): {AbsX: 10, AbsY: 20},
		pixelKey(11, 21): {AbsX: 11, AbsY: 21},
		pixelKey(12, 22): {AbsX: 12, AbsY: 22},
	}
	baseline := map[string]Pixel{
		pixelKey(10, 20): {AbsX: 10, AbsY: 20},
	}
	vs := &VandalState{PixelToPainter: map[string]string{
		pixelKey(10, 20): "base-user",
	}}

	claimed := claimCurrentDiffPixels(current, "1001", vs, baseline)
	if claimed != 2 {
		t.Fatalf("expected claimed pixels=2 (excluding baseline), got %d", claimed)
	}
	if got := vs.PixelToPainter[pixelKey(10, 20)]; got != "base-user" {
		t.Fatalf("baseline pixel should be untouched, got %q", got)
	}
	if got := vs.PixelToPainter[pixelKey(11, 21)]; got != "1001" {
		t.Fatalf("unexpected painter for pixel(11,21): %q", got)
	}
	if got := vs.PixelToPainter[pixelKey(12, 22)]; got != "1001" {
		t.Fatalf("unexpected painter for pixel(12,22): %q", got)
	}
}

func TestUpdateDiffImageInferenceQueuesSingleProbe(t *testing.T) {
	t.Parallel()

	tracker := NewTracker(Config{
		TopLeftTileX:  0,
		TopLeftTileY:  0,
		TopLeftPixelX: 0,
		TopLeftPixelY: 0,
		Width:         2,
		Height:        2,
	}, nil, "")

	first := mustEncodeDiffPNG(t, map[[2]int]bool{
		{0, 0}: true,
		{1, 0}: true,
		{0, 1}: true,
	})
	if err := tracker.UpdateDiffImage(first); err != nil {
		t.Fatalf("UpdateDiffImage(first) returned error: %v", err)
	}

	if got := len(tracker.queue); got != 1 {
		t.Fatalf("expected queue length 1 (probe only), got %d", got)
	}

	tracker.mu.Lock()
	if !tracker.vandalInference.ProbeQueued {
		t.Fatalf("expected ProbeQueued=true after first update")
	}
	if got := tracker.vandalInference.RemainingPixels; got != 3 {
		t.Fatalf("expected remaining inference pixels 3, got %d", got)
	}
	tracker.mu.Unlock()

	second := mustEncodeDiffPNG(t, map[[2]int]bool{
		{0, 0}: true,
		{1, 0}: true,
		{0, 1}: true,
		{1, 1}: true, // +1 added while probe still pending
	})
	if err := tracker.UpdateDiffImage(second); err != nil {
		t.Fatalf("UpdateDiffImage(second) returned error: %v", err)
	}

	if got := len(tracker.queue); got != 1 {
		t.Fatalf("expected still 1 queued probe after second update, got %d", got)
	}
	tracker.mu.Lock()
	if got := tracker.vandalInference.RemainingPixels; got != 4 {
		t.Fatalf("expected remaining inference pixels to track current diff(4), got %d", got)
	}
	tracker.mu.Unlock()
}

func TestUpdateDiffImageInferenceAutoArmsOnMonotonicGrowth(t *testing.T) {
	t.Parallel()

	tracker := NewTracker(Config{
		TopLeftTileX:  0,
		TopLeftTileY:  0,
		TopLeftPixelX: 0,
		TopLeftPixelY: 0,
		Width:         3,
		Height:        3,
	}, nil, "")

	first := mustEncodeDiffPNG(t, map[[2]int]bool{
		{0, 0}: true,
	})
	if err := tracker.UpdateDiffImage(first); err != nil {
		t.Fatalf("UpdateDiffImage(first) returned error: %v", err)
	}

	tracker.mu.Lock()
	if tracker.vandalInference.Active {
		t.Fatalf("inference should not auto-arm for added=1")
	}
	tracker.mu.Unlock()

	second := mustEncodeDiffPNG(t, map[[2]int]bool{
		{0, 0}: true,
		{1, 0}: true,
		{0, 1}: true,
	})
	if err := tracker.UpdateDiffImage(second); err != nil {
		t.Fatalf("UpdateDiffImage(second) returned error: %v", err)
	}

	tracker.mu.Lock()
	if !tracker.vandalInference.Active {
		t.Fatalf("expected inference auto-arm on monotonic growth")
	}
	if !tracker.vandalInference.ProbeQueued {
		t.Fatalf("expected single probe to be queued")
	}
	if got := tracker.vandalInference.BaselinePixels; got != 1 {
		t.Fatalf("expected baseline pixels=1, got %d", got)
	}
	if got := tracker.vandalInference.RemainingPixels; got != 2 {
		t.Fatalf("expected remaining inferred pixels=2, got %d", got)
	}
	tracker.mu.Unlock()
}

func TestUpdateDiffImageInferenceResetsWhenRestoreAppears(t *testing.T) {
	t.Parallel()

	tracker := NewTracker(Config{
		TopLeftTileX:  0,
		TopLeftTileY:  0,
		TopLeftPixelX: 0,
		TopLeftPixelY: 0,
		Width:         3,
		Height:        3,
	}, nil, "")

	first := mustEncodeDiffPNG(t, map[[2]int]bool{
		{0, 0}: true,
		{1, 0}: true,
		{0, 1}: true,
	})
	if err := tracker.UpdateDiffImage(first); err != nil {
		t.Fatalf("UpdateDiffImage(first) returned error: %v", err)
	}

	tracker.mu.Lock()
	if !tracker.vandalInference.Active {
		t.Fatalf("expected inference active after first update")
	}
	tracker.mu.Unlock()

	// remove one vandalized pixel -> inference assumption must be cleared.
	second := mustEncodeDiffPNG(t, map[[2]int]bool{
		{0, 0}: true,
		{1, 0}: true,
	})
	if err := tracker.UpdateDiffImage(second); err != nil {
		t.Fatalf("UpdateDiffImage(second) returned error: %v", err)
	}

	tracker.mu.Lock()
	if tracker.vandalInference.Active {
		t.Fatalf("expected inference reset when restore appears")
	}
	tracker.mu.Unlock()
}

func TestUpdateDiffImageRestoreInferenceAutoArmsOnMonotonicShrink(t *testing.T) {
	t.Parallel()

	tracker := NewTracker(Config{
		TopLeftTileX:  0,
		TopLeftTileY:  0,
		TopLeftPixelX: 0,
		TopLeftPixelY: 0,
		Width:         3,
		Height:        3,
	}, nil, "")

	tracker.mu.Lock()
	tracker.currentDiff = map[string]Pixel{
		pixelKey(0, 0): {AbsX: 0, AbsY: 0},
		pixelKey(1, 0): {AbsX: 1, AbsY: 0},
		pixelKey(0, 1): {AbsX: 0, AbsY: 1},
	}
	tracker.mu.Unlock()

	shrink := mustEncodeDiffPNG(t, map[[2]int]bool{
		{0, 0}: true,
	})
	if err := tracker.UpdateDiffImage(shrink); err != nil {
		t.Fatalf("UpdateDiffImage(shrink) returned error: %v", err)
	}

	if got := len(tracker.queue); got != 1 {
		t.Fatalf("expected queue length 1 (restore probe only), got %d", got)
	}

	tracker.mu.Lock()
	if !tracker.restoreInference.Active {
		t.Fatalf("expected restore inference auto-arm on monotonic shrink")
	}
	if !tracker.restoreInference.ProbeQueued {
		t.Fatalf("expected restore probe to be queued")
	}
	if got := tracker.restoreInference.BaselinePixels; got != 3 {
		t.Fatalf("expected restore baseline pixels=3, got %d", got)
	}
	if got := tracker.restoreInference.RemainingPixels; got != 2 {
		t.Fatalf("expected remaining restored pixels=2, got %d", got)
	}
	tracker.mu.Unlock()
}

func TestUpdateDiffImageRestoreInferenceResetsWhenVandalAppears(t *testing.T) {
	t.Parallel()

	tracker := NewTracker(Config{
		TopLeftTileX:  0,
		TopLeftTileY:  0,
		TopLeftPixelX: 0,
		TopLeftPixelY: 0,
		Width:         3,
		Height:        3,
	}, nil, "")

	tracker.mu.Lock()
	tracker.currentDiff = map[string]Pixel{
		pixelKey(0, 0): {AbsX: 0, AbsY: 0},
		pixelKey(1, 0): {AbsX: 1, AbsY: 0},
		pixelKey(0, 1): {AbsX: 0, AbsY: 1},
	}
	tracker.mu.Unlock()

	shrink := mustEncodeDiffPNG(t, map[[2]int]bool{
		{0, 0}: true,
	})
	if err := tracker.UpdateDiffImage(shrink); err != nil {
		t.Fatalf("UpdateDiffImage(shrink) returned error: %v", err)
	}

	tracker.mu.Lock()
	if !tracker.restoreInference.Active {
		t.Fatalf("expected restore inference active after shrink")
	}
	tracker.mu.Unlock()

	// add a new vandal diff while restore inference is pending -> inference must be cleared.
	growth := mustEncodeDiffPNG(t, map[[2]int]bool{
		{0, 0}: true,
		{2, 2}: true,
	})
	if err := tracker.UpdateDiffImage(growth); err != nil {
		t.Fatalf("UpdateDiffImage(growth) returned error: %v", err)
	}

	tracker.mu.Lock()
	if tracker.restoreInference.Active {
		t.Fatalf("expected restore inference reset when added diff appears")
	}
	tracker.mu.Unlock()
}

func TestUpdateDiffImageUnchangedDiffDoesNotMarkDirty(t *testing.T) {
	t.Parallel()

	tracker := NewTracker(Config{Width: 2, Height: 2}, nil, "")
	clearDirty := func() {
		tracker.mu.Lock()
		tracker.dirtyVandalState = false
		tracker.dirtyDailyCounts = false
		tracker.mu.Unlock()
	}
	dirty := func() (bool, bool) {
		tracker.mu.Lock()
		defer tracker.mu.Unlock()
		return tracker.dirtyVandalState, tracker.dirtyDailyCounts
	}

	empty := mustEncodeDiffPNG(t, nil)
	if err := tracker.UpdateDiffImage(empty); err != nil {
		t.Fatalf("UpdateDiffImage(empty) returned error: %v", err)
	}
	clearDirty()

	if err := tracker.UpdateDiffImage(empty); err != nil {
		t.Fatalf("UpdateDiffImage(empty again) returned error: %v", err)
	}
	if vandal, daily := dirty(); vandal || daily {
		t.Fatalf("unchanged diff must not mark state dirty: vandal=%v daily=%v", vandal, daily)
	}

	changed := mustEncodeDiffPNG(t, map[[2]int]bool{{0, 0}: true})
	if err := tracker.UpdateDiffImage(changed); err != nil {
		t.Fatalf("UpdateDiffImage(changed) returned error: %v", err)
	}
	if vandal, daily := dirty(); !vandal || !daily {
		t.Fatalf("changed diff must mark state dirty: vandal=%v daily=%v", vandal, daily)
	}
}

func mustEncodeDiffPNG(t *testing.T, pixels map[[2]int]bool) []byte {
	t.Helper()

	maxX := 1
	maxY := 1
	for pos := range pixels {
		if pos[0] > maxX {
			maxX = pos[0]
		}
		if pos[1] > maxY {
			maxY = pos[1]
		}
	}
	img := image.NewNRGBA(image.Rect(0, 0, maxX+1, maxY+1))
	for pos := range pixels {
		img.Set(pos[0], pos[1], color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("failed to encode diff png: %v", err)
	}
	return buf.Bytes()
}
