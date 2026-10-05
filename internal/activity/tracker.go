package activity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"Koukyo_discord_bot/internal/utils"
)

type Config struct {
	TopLeftTileX  int
	TopLeftTileY  int
	TopLeftPixelX int
	TopLeftPixelY int
	Width         int
	Height        int
}

type Pixel struct {
	AbsX int
	AbsY int
}

type PaintedBy struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	AllianceID   int    `json:"allianceId"`
	AllianceName string `json:"allianceName"`
	EquippedFlag int    `json:"equippedFlag"`
	Picture      string `json:"picture"`
	Discord      string `json:"discord"`
	DiscordID    string `json:"discordId"`
	Banned       bool   `json:"banned"`
	TimedOut     bool   `json:"timedOut"`
}

type PixelRef struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type UserActivity struct {
	ID                  string         `json:"id"`
	Name                string         `json:"name"`
	AllianceID          int            `json:"allianceId,omitempty"`
	AllianceName        string         `json:"allianceName"`
	Discord             string         `json:"discord,omitempty"`
	DiscordID           string         `json:"discord_id,omitempty"`
	Picture             string         `json:"picture,omitempty"`
	LastSeen            string         `json:"last_seen"`
	VandalCount         int            `json:"vandal_count"`
	RestoredCount       int            `json:"restored_count"`
	ActivityScore       int            `json:"activity_score"`
	DailyVandalCounts   map[string]int `json:"daily_vandal_counts,omitempty"`
	DailyRestoredCounts map[string]int `json:"daily_restored_counts,omitempty"`
	DailyActivityScores map[string]int `json:"daily_activity_scores,omitempty"`
	LastPixel           *PixelRef      `json:"last_pixel,omitempty"`
	VandalNotified      bool           `json:"vandal_notified,omitempty"`
	FixNotified         bool           `json:"fix_notified,omitempty"`
}

type PainterPixelCount struct {
	UserID string `json:"user_id"`
	Name   string `json:"name,omitempty"`
	Pixels int    `json:"pixels"`
}

type VandalState struct {
	VandalizedPixels [][]int           `json:"vandalized_pixels"`
	PixelToPainter   map[string]string `json:"pixel_to_painter"`
}

type DailyPixelCounts struct {
	Vandal map[string]int `json:"vandal"`
	Fix    map[string]int `json:"fix"`
}

const (
	newUserNotifyThreshold      = 5
	newUserNotifyWindow         = 5 * time.Minute
	powerSaveInferenceMinPixels = 2
	powerSaveInferenceTTL       = 2 * time.Minute
	defaultStateFlushInterval   = 10 * time.Second
	defaultRecentEventsInterval = 1 * time.Minute
	defaultActivityGCInterval   = 24 * time.Hour
	activityRetentionDays       = 36500 // 約100年（実質的に半永久保持）
)

type powerSaveInferenceState struct {
	Active          bool
	ProbeQueued     bool
	ClaimedPainter  string
	RemainingPixels int
	BaselinePixels  int
	ExpiresAt       time.Time
	Baseline        map[string]Pixel
}

type Tracker struct {
	cfg          Config
	limiter      *utils.RateLimiter
	dataDir      string
	httpClient   *http.Client
	newUserCB    NewUserCallback
	queue        chan Pixel
	pending      map[string]Pixel
	diffQueue    chan []byte
	ctx          context.Context
	cancel       context.CancelFunc
	mu           sync.Mutex
	currentDiff  map[string]Pixel
	activity     map[string]*UserActivity
	vandalState  VandalState
	dailyCounts  DailyPixelCounts
	backoffDelay time.Duration
	backoffUntil time.Time
	// Recent event windows for "N actions within 5 minutes" detection.
	recentVandalEvents map[string][]time.Time
	recentFixEvents    map[string][]time.Time
	dirtyActivity      bool
	dirtyVandalState   bool
	dirtyDailyCounts   bool
	flushInterval      time.Duration
	recentGCInterval   time.Duration
	activityGCInterval time.Duration
	powerSaveInference powerSaveInferenceState
	restoreInference   powerSaveInferenceState
}

type NewUserCallback func(kind string, user UserActivity)

var activityDebugLogging = os.Getenv("ACTIVITY_DEBUG_LOG") == "1"

func activityDebugf(format string, args ...interface{}) {
	if !activityDebugLogging {
		return
	}
	log.Printf(format, args...)
}

func NewTracker(cfg Config, limiter *utils.RateLimiter, dataDir string) *Tracker {
	ctx, cancel := context.WithCancel(context.Background())
	queueSize := cfg.Width * cfg.Height
	if queueSize <= 0 {
		queueSize = 4096
	}
	diffQueueSize := 1
	t := &Tracker{
		cfg:                cfg,
		limiter:            limiter,
		dataDir:            dataDir,
		httpClient:         NewPixelHTTPClient(),
		queue:              make(chan Pixel, queueSize),
		pending:            make(map[string]Pixel),
		diffQueue:          make(chan []byte, diffQueueSize),
		ctx:                ctx,
		cancel:             cancel,
		currentDiff:        make(map[string]Pixel),
		activity:           make(map[string]*UserActivity),
		vandalState:        VandalState{PixelToPainter: make(map[string]string)},
		backoffDelay:       2 * time.Second,
		recentVandalEvents: make(map[string][]time.Time),
		recentFixEvents:    make(map[string][]time.Time),
		flushInterval:      loadDurationFromEnv("ACTIVITY_FLUSH_INTERVAL_SECONDS", defaultStateFlushInterval, time.Second, 10*time.Minute),
		recentGCInterval:   loadDurationFromEnv("ACTIVITY_RECENT_GC_INTERVAL_SECONDS", defaultRecentEventsInterval, 10*time.Second, 10*time.Minute),
		activityGCInterval: loadDurationFromEnv("ACTIVITY_GC_INTERVAL_SECONDS", defaultActivityGCInterval, 1*time.Hour, 7*24*time.Hour),
	}
	t.loadState()
	t.loadDailyCounts()
	return t
}

func (t *Tracker) SetNewUserCallback(cb NewUserCallback) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.newUserCB = cb
}

func (t *Tracker) Start() {
	go t.runWorker("worker", t.worker)
	go t.runWorker("diffWorker", t.diffWorker)
	go t.runWorker("flushWorker", t.flushWorker)
	go t.runWorker("recentEventsGCWorker", t.recentEventsGCWorker)
	go t.runWorker("activityGCWorker", t.activityGCWorker)
}

func (t *Tracker) Stop() {
	t.cancel()
}

func (t *Tracker) GetCurrentDiffPainterCounts(limit int) []PainterPixelCount {
	if t == nil {
		return nil
	}

	t.mu.Lock()
	if len(t.vandalState.PixelToPainter) == 0 {
		t.mu.Unlock()
		return nil
	}
	countsByUser := make(map[string]int, len(t.vandalState.PixelToPainter))
	for _, userID := range t.vandalState.PixelToPainter {
		if userID == "" {
			continue
		}
		countsByUser[userID]++
	}
	nameByUser := make(map[string]string, len(countsByUser))
	for userID := range countsByUser {
		if entry := t.activity[userID]; entry != nil {
			nameByUser[userID] = entry.Name
		}
	}
	t.mu.Unlock()

	list := make([]PainterPixelCount, 0, len(countsByUser))
	for userID, pixels := range countsByUser {
		list = append(list, PainterPixelCount{
			UserID: userID,
			Name:   nameByUser[userID],
			Pixels: pixels,
		})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Pixels == list[j].Pixels {
			return list[i].UserID < list[j].UserID
		}
		return list[i].Pixels > list[j].Pixels
	})
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}
	return list
}

func (t *Tracker) runWorker(name string, fn func()) {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("PANIC in activity %s: %v", name, r)
				}
			}()
			fn()
		}()

		select {
		case <-t.ctx.Done():
			return
		default:
		}
		log.Printf("activity %s stopped unexpectedly; restarting in 1s", name)
		time.Sleep(1 * time.Second)
	}
}

func (t *Tracker) EnqueueDiffImage(pngBytes []byte) {
	if len(pngBytes) == 0 {
		return
	}
	select {
	case t.diffQueue <- pngBytes:
	default:
		select {
		case <-t.diffQueue:
		default:
		}
		select {
		case t.diffQueue <- pngBytes:
		default:
		}
	}
}

func (t *Tracker) UpdateDiffImage(pngBytes []byte) error {
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return err
	}
	bounds := img.Bounds()
	activityDebugf("activity diff image size: %dx%d", bounds.Dx(), bounds.Dy())
	baseAbsX := t.cfg.TopLeftTileX*utils.WplaceTileSize + t.cfg.TopLeftPixelX
	baseAbsY := t.cfg.TopLeftTileY*utils.WplaceTileSize + t.cfg.TopLeftPixelY

	newDiff := make(map[string]Pixel)
	nonZero := 0

	// Optimize by using direct pixel access if possible
	if nrgba, ok := img.(*image.NRGBA); ok {
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				i := nrgba.PixOffset(x, y)
				if nrgba.Pix[i+3] == 0 {
					continue
				}
				nonZero++
				absX := baseAbsX + (x - bounds.Min.X)
				absY := baseAbsY + (y - bounds.Min.Y)
				key := pixelKey(absX, absY)
				newDiff[key] = Pixel{AbsX: absX, AbsY: absY}
			}
		}
	} else if rgba, ok := img.(*image.RGBA); ok {
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				i := rgba.PixOffset(x, y)
				if rgba.Pix[i+3] == 0 {
					continue
				}
				nonZero++
				absX := baseAbsX + (x - bounds.Min.X)
				absY := baseAbsY + (y - bounds.Min.Y)
				key := pixelKey(absX, absY)
				newDiff[key] = Pixel{AbsX: absX, AbsY: absY}
			}
		}
	} else {
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				_, _, _, a := img.At(x, y).RGBA()
				if a == 0 {
					continue
				}
				nonZero++
				absX := baseAbsX + (x - bounds.Min.X)
				absY := baseAbsY + (y - bounds.Min.Y)
				key := pixelKey(absX, absY)
				newDiff[key] = Pixel{AbsX: absX, AbsY: absY}
			}
		}
	}
	activityDebugf("activity diff pixels detected: %d", nonZero)

	t.mu.Lock()
	oldDiff := t.currentDiff
	t.currentDiff = newDiff
	pruned := 0
	for key := range t.vandalState.PixelToPainter {
		if _, ok := newDiff[key]; !ok {
			delete(t.vandalState.PixelToPainter, key)
			pruned++
		}
	}
	prevVandalized := len(t.vandalState.VandalizedPixels)
	t.vandalState.VandalizedPixels = diffPixelsToList(newDiff)
	added, removed := countDiffChanges(oldDiff, newDiff)
	dateKey := dateKeyJST()
	_, hadDailyEntry := t.dailyCounts.Vandal[dateKey]
	if t.dailyCounts.Vandal == nil {
		t.dailyCounts.Vandal = make(map[string]int)
	}
	if t.dailyCounts.Fix == nil {
		t.dailyCounts.Fix = make(map[string]int)
	}
	t.dailyCounts.Vandal[dateKey] += added
	t.dailyCounts.Fix[dateKey] += removed
	// Diff images arrive continuously; only flush to disk when something changed.
	changed := added > 0 || removed > 0
	if changed || pruned > 0 || prevVandalized != len(newDiff) {
		t.dirtyVandalState = true
	}
	if changed || !hadDailyEntry {
		t.dirtyDailyCounts = true
	}
	addedPixels := make([]Pixel, 0, added)
	for key, px := range newDiff {
		if _, ok := oldDiff[key]; !ok {
			addedPixels = append(addedPixels, px)
		}
	}
	removedPixels := make([]Pixel, 0, removed)
	for key, px := range oldDiff {
		if _, ok := newDiff[key]; !ok {
			removedPixels = append(removedPixels, px)
		}
	}

	queueAdded := addedPixels
	queueRemoved := removedPixels
	now := time.Now().UTC()
	if t.powerSaveInference.Active && now.After(t.powerSaveInference.ExpiresAt) {
		resetPowerSaveInference(&t.powerSaveInference)
	}
	if t.restoreInference.Active && now.After(t.restoreInference.ExpiresAt) {
		resetPowerSaveInference(&t.restoreInference)
	}
	if !t.powerSaveInference.Active && shouldAutoArmPowerSaveInference(added, removed) {
		if armPowerSaveInferenceFromGrowth(&t.powerSaveInference, oldDiff, added, now) {
			activityDebugf("activity inference auto-armed from monotonic growth: added=%d removed=%d baseline=%d", added, removed, len(oldDiff))
		}
	}
	if !t.restoreInference.Active && shouldAutoArmRestoreInference(added, removed) {
		if armRestoreInferenceFromShrink(&t.restoreInference, oldDiff, removed, now) {
			activityDebugf("activity restore inference auto-armed from monotonic shrink: added=%d removed=%d baseline=%d", added, removed, len(oldDiff))
		}
	}
	if t.powerSaveInference.Active {
		if t.powerSaveInference.ClaimedPainter == "" {
			if removed > 0 {
				// Monotonic-growth assumption is broken once restores appear.
				resetPowerSaveInference(&t.powerSaveInference)
			} else {
				// While inference is active and not yet claimed, keep queue/API to one probe.
				queueAdded = nil
				updatePowerSaveInferenceRemaining(&t.powerSaveInference, newDiff)
				if t.powerSaveInference.RemainingPixels <= 0 {
					resetPowerSaveInference(&t.powerSaveInference)
				} else if !t.powerSaveInference.ProbeQueued {
					probe, ok := chooseInferenceProbePixel(newDiff, addedPixels, t.pending, t.powerSaveInference.Baseline)
					if ok {
						queueAdded = []Pixel{probe}
						t.powerSaveInference.ProbeQueued = true
					}
				}
			}
		} else {
			// Painter already determined: no further queue/API for added pixels.
			claimCurrentDiffPixels(t.currentDiff, t.powerSaveInference.ClaimedPainter, &t.vandalState, t.powerSaveInference.Baseline)
			queueAdded = nil
		}
	}
	if t.restoreInference.Active {
		if t.restoreInference.ClaimedPainter == "" {
			if added > 0 {
				// Monotonic-shrink assumption is broken once new vandal diffs appear.
				resetPowerSaveInference(&t.restoreInference)
			} else {
				// While restore inference is active and not yet claimed, keep queue/API to one probe.
				queueRemoved = nil
				updateRestoreInferenceRemaining(&t.restoreInference, newDiff)
				if t.restoreInference.RemainingPixels <= 0 {
					resetPowerSaveInference(&t.restoreInference)
				} else if !t.restoreInference.ProbeQueued {
					probe, ok := chooseRestoreInferenceProbePixel(newDiff, removedPixels, t.pending, t.restoreInference.Baseline)
					if ok {
						queueRemoved = []Pixel{probe}
						t.restoreInference.ProbeQueued = true
					}
				}
			}
		} else {
			// Restorer already determined: no further queue/API for removed pixels.
			queueRemoved = nil
		}
	}
	t.mu.Unlock()

	if len(queueAdded) == 0 && len(queueRemoved) == 0 {
		activityDebugf("activity diff changes: none")
	}

	for _, px := range queueAdded {
		t.enqueuePixel(px)
	}
	for _, px := range queueRemoved {
		t.enqueuePixel(px)
	}

	return nil
}

func (t *Tracker) worker() {
	for {
		select {
		case <-t.ctx.Done():
			return
		case px := <-t.queue:
			t.processPixel(px)
		}
	}
}

func (t *Tracker) diffWorker() {
	for {
		select {
		case <-t.ctx.Done():
			return
		case diff := <-t.diffQueue:
			if err := t.UpdateDiffImage(diff); err != nil {
				log.Printf("activity diff update failed: %v", err)
			}
		}
	}
}

func (t *Tracker) processPixel(px Pixel) {
	key := pixelKey(px.AbsX, px.AbsY)

	t.mu.Lock()
	_, isDiff := t.currentDiff[key]
	t.mu.Unlock()
	activityDebugf("activity process pixel: %s (isDiff=%t)", key, isDiff)
	defer func() {
		t.mu.Lock()
		delete(t.pending, key)
		t.mu.Unlock()
	}()

	painter, err := t.fetchPainter(px)
	if err != nil {
		log.Printf("activity fetch error for %s: %v", key, err)
		t.mu.Lock()
		clearInferenceProbeOnFetchFailure(&t.powerSaveInference, &t.restoreInference)
		t.mu.Unlock()
		return
	}
	if painter == nil {
		activityDebugf("activity fetch painter: nil for %s", key)
		t.mu.Lock()
		clearInferenceProbeOnFetchFailure(&t.powerSaveInference, &t.restoreInference)
		t.mu.Unlock()
		return
	}

	now := time.Now().UTC()
	jst := time.FixedZone("JST", 9*3600)
	dateKey := now.In(jst).Format("2006-01-02")

	t.mu.Lock()
	if !isDiff && t.powerSaveInference.Active && t.powerSaveInference.ClaimedPainter == "" && t.powerSaveInference.ProbeQueued {
		// Probe target changed to non-diff before painter fetch completed; allow next probe.
		t.powerSaveInference.ProbeQueued = false
	}
	if isDiff && t.restoreInference.Active && t.restoreInference.ClaimedPainter == "" && t.restoreInference.ProbeQueued {
		// Restore probe target became diff again before fetch completed; allow next probe.
		t.restoreInference.ProbeQueued = false
	}
	detectedPainterID := strconv.Itoa(painter.ID)
	effectivePainterID := detectedPainterID
	inferenceActive := false
	inferenceCredit := 0
	restoreInferenceActive := false
	restoreInferenceCredit := 0
	if isDiff {
		inferenceActive, effectivePainterID, inferenceCredit = beginPowerSaveInference(&t.powerSaveInference, detectedPainterID, now)
	} else {
		restoreInferenceActive, effectivePainterID, restoreInferenceCredit = beginPowerSaveInference(&t.restoreInference, detectedPainterID, now)
	}

	entry := t.activity[effectivePainterID]
	if entry == nil {
		entry = &UserActivity{
			ID:   effectivePainterID,
			Name: fmt.Sprintf("ID:%s", effectivePainterID),
		}
		ensureActivityMaps(entry)
		t.activity[effectivePainterID] = entry
	} else {
		ensureActivityMaps(entry)
	}

	// Keep profile fields trusted: only overwrite when we are updating the
	// actually detected painter, not an inferred/aliased one.
	if effectivePainterID == detectedPainterID {
		if painter.Name != "" {
			entry.Name = painter.Name
		}
		if painter.AllianceID != 0 {
			entry.AllianceID = painter.AllianceID
		}
		if painter.AllianceName != "" {
			entry.AllianceName = painter.AllianceName
		}
		if painter.Discord != "" {
			entry.Discord = painter.Discord
		}
		if painter.DiscordID != "" {
			entry.DiscordID = painter.DiscordID
		}
		if painter.Picture != "" {
			entry.Picture = painter.Picture
		}
	}

	entry.LastSeen = now.Format(time.RFC3339Nano)
	entry.LastPixel = &PixelRef{X: px.AbsX, Y: px.AbsY}

	notifyKind := ""
	shouldNotify := false
	if isDiff {
		if inferenceActive {
			t.vandalState.PixelToPainter[key] = effectivePainterID
			if inferenceCredit > 0 {
				assigned := claimCurrentDiffPixels(t.currentDiff, effectivePainterID, &t.vandalState, t.powerSaveInference.Baseline)
				credited := inferenceCredit
				if assigned > 0 {
					credited = assigned
				}
				entry.VandalCount += credited
				entry.DailyVandalCounts[dateKey] += credited
				entry.ActivityScore -= credited
				entry.DailyActivityScores[dateKey] -= credited
				windowCount := recordRecentEvents(t.recentVandalEvents, effectivePainterID, now, newUserNotifyWindow, credited)
				if !entry.VandalNotified && windowCount >= newUserNotifyThreshold {
					notifyKind = "vandal"
					shouldNotify = true
					entry.VandalNotified = true
				}
				activityDebugf("activity inference current diff assigned=%d", assigned)
				log.Printf("activity inference claimed painter=%s pixels=%d", effectivePainterID, credited)
				// Inference is consumed in a single claim (1 API call).
				resetPowerSaveInference(&t.powerSaveInference)
			} else {
				consumePowerSaveInference(&t.powerSaveInference)
			}
			if effectivePainterID != detectedPainterID {
				activityDebugf("activity inference aliases %s -> %s", detectedPainterID, effectivePainterID)
			}
		} else {
			entry.VandalCount++
			entry.DailyVandalCounts[dateKey]++
			entry.ActivityScore--
			entry.DailyActivityScores[dateKey]--
			t.vandalState.PixelToPainter[key] = effectivePainterID
			windowCount := recordRecentEvent(t.recentVandalEvents, effectivePainterID, now, newUserNotifyWindow)
			if !entry.VandalNotified && windowCount >= newUserNotifyThreshold {
				notifyKind = "vandal"
				shouldNotify = true
				entry.VandalNotified = true
			}
		}
	} else {
		if restoreInferenceActive {
			if restoreInferenceCredit > 0 {
				credited := restoreInferenceCredit
				restored := countRemovedFromBaseline(t.currentDiff, t.restoreInference.Baseline)
				if restored > 0 {
					credited = restored
				}
				entry.RestoredCount += credited
				entry.DailyRestoredCounts[dateKey] += credited
				entry.ActivityScore += credited
				entry.DailyActivityScores[dateKey] += credited
				windowCount := recordRecentEvents(t.recentFixEvents, effectivePainterID, now, newUserNotifyWindow, credited)
				if !entry.FixNotified && windowCount >= newUserNotifyThreshold {
					notifyKind = "fix"
					shouldNotify = true
					entry.FixNotified = true
				}
				log.Printf("activity restore inference claimed painter=%s pixels=%d", effectivePainterID, credited)
				// Inference is consumed in a single claim (1 API call).
				resetPowerSaveInference(&t.restoreInference)
			} else {
				consumePowerSaveInference(&t.restoreInference)
			}
			if effectivePainterID != detectedPainterID {
				activityDebugf("activity restore inference aliases %s -> %s", detectedPainterID, effectivePainterID)
			}
		} else {
			entry.RestoredCount++
			entry.DailyRestoredCounts[dateKey]++
			entry.ActivityScore++
			entry.DailyActivityScores[dateKey]++
			windowCount := recordRecentEvent(t.recentFixEvents, effectivePainterID, now, newUserNotifyWindow)
			if !entry.FixNotified && windowCount >= newUserNotifyThreshold {
				notifyKind = "fix"
				shouldNotify = true
				entry.FixNotified = true
			}
		}
		delete(t.vandalState.PixelToPainter, key)
	}

	t.dirtyActivity = true
	t.dirtyVandalState = true
	cb := t.newUserCB
	var userCopy UserActivity
	if shouldNotify {
		userCopy = cloneUserActivity(entry)
	}
	t.mu.Unlock()

	if shouldNotify && cb != nil {
		cb(notifyKind, userCopy)
	}
}

func (t *Tracker) fetchPainter(px Pixel) (*PaintedBy, error) {
	if err := t.waitForBackoff(); err != nil {
		return nil, err
	}
	tileX := px.AbsX / utils.WplaceTileSize
	tileY := px.AbsY / utils.WplaceTileSize
	pixelX := px.AbsX % utils.WplaceTileSize
	pixelY := px.AbsY % utils.WplaceTileSize
	parsed, status, err := FetchPixelInfo(t.ctx, t.httpClient, t.limiter, tileX, tileY, pixelX, pixelY)
	if err != nil {
		if status == http.StatusTooManyRequests {
			t.markBackoff()
		}
		return nil, err
	}
	t.resetBackoff()
	if parsed == nil || parsed.PaintedBy == nil || parsed.PaintedBy.ID == 0 {
		return nil, nil
	}
	return parsed.PaintedBy, nil
}

func (t *Tracker) waitForBackoff() error {
	t.mu.Lock()
	until := t.backoffUntil
	t.mu.Unlock()
	if until.IsZero() {
		return nil
	}
	delay := time.Until(until)
	if delay <= 0 {
		return nil
	}
	select {
	case <-t.ctx.Done():
		return t.ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

func (t *Tracker) markBackoff() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.backoffDelay <= 0 {
		t.backoffDelay = 2 * time.Second
	}
	if t.backoffDelay > 30*time.Second {
		t.backoffDelay = 30 * time.Second
	}
	t.backoffUntil = time.Now().Add(t.backoffDelay)
	t.backoffDelay *= 2
}

func (t *Tracker) resetBackoff() {
	t.mu.Lock()
	t.backoffDelay = 2 * time.Second
	t.backoffUntil = time.Time{}
	t.mu.Unlock()
}
func (t *Tracker) loadState() {
	activityPath := filepath.Join(t.dataDir, "user_activity.json")
	var entries map[string]*UserActivity
	source, err := utils.ReadJSONFileWithBackup(activityPath, &entries)
	switch {
	case err == nil:
		dirty := source == utils.BackupPath(activityPath)
		for _, entry := range entries {
			expectedScore := entry.RestoredCount - entry.VandalCount
			if entry.DailyActivityScores == nil || entry.ActivityScore != expectedScore {
				dirty = true
			}
			ensureActivityMaps(entry)
			entry.ActivityScore = expectedScore
			entry.DailyActivityScores = buildDailyActivityScores(entry)
			if entry.VandalCount >= newUserNotifyThreshold && !entry.VandalNotified {
				entry.VandalNotified = true
				dirty = true
			}
			if entry.RestoredCount >= newUserNotifyThreshold && !entry.FixNotified {
				entry.FixNotified = true
				dirty = true
			}
		}
		if entries != nil {
			t.activity = entries
		}
		if dirty {
			if err := t.saveActivitySnapshot(); err != nil {
				log.Printf("failed to migrate user activity: %v", err)
			}
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		log.Printf("failed to load user activity: %v", err)
	}

	vandalPath := filepath.Join(t.dataDir, "vandalized_pixels.json")
	var state VandalState
	source, err = utils.ReadJSONFileWithBackup(vandalPath, &state)
	switch {
	case err == nil:
		if state.PixelToPainter == nil {
			state.PixelToPainter = make(map[string]string)
		}
		t.vandalState = state
		if source == utils.BackupPath(vandalPath) {
			t.dirtyVandalState = true
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		log.Printf("failed to load vandal state: %v", err)
	}
}

func (t *Tracker) loadDailyCounts() {
	path := filepath.Join(t.dataDir, "vandal_daily.json")
	var counts DailyPixelCounts
	source, err := utils.ReadJSONFileWithBackup(path, &counts)
	switch {
	case err == nil:
		if counts.Vandal == nil {
			counts.Vandal = make(map[string]int)
		}
		if counts.Fix == nil {
			counts.Fix = make(map[string]int)
		}
		t.dailyCounts = counts
		if source == utils.BackupPath(path) {
			t.dirtyDailyCounts = true
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		log.Printf("failed to load daily counts: %v", err)
	}
}

func (t *Tracker) saveActivitySnapshot() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return saveUserActivityMap(t.dataDir, t.activity)
}

func (t *Tracker) flushWorker() {
	ticker := time.NewTicker(t.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-t.ctx.Done():
			t.flushDirtyState()
			return
		case <-ticker.C:
			t.flushDirtyState()
		}
	}
}

func (t *Tracker) flushDirtyState() {
	type filePayload struct {
		name string
		data []byte
	}
	payloads := make([]filePayload, 0, 2)
	var activitySnapshot map[string]*UserActivity
	flushActivity := false

	t.mu.Lock()
	flushActivity = t.dirtyActivity
	flushVandal := t.dirtyVandalState
	flushDaily := t.dirtyDailyCounts
	if flushActivity {
		activitySnapshot = make(map[string]*UserActivity, len(t.activity))
		for id, entry := range t.activity {
			activitySnapshot[id] = entry
		}
	}
	if flushVandal {
		if data, err := json.MarshalIndent(t.vandalState, "", "  "); err != nil {
			log.Printf("failed to marshal vandal state: %v", err)
		} else {
			payloads = append(payloads, filePayload{name: "vandalized_pixels.json", data: data})
			t.dirtyVandalState = false
		}
	}
	if flushDaily {
		if data, err := json.MarshalIndent(t.dailyCounts, "", "  "); err != nil {
			log.Printf("failed to marshal daily counts: %v", err)
		} else {
			payloads = append(payloads, filePayload{name: "vandal_daily.json", data: data})
			t.dirtyDailyCounts = false
		}
	}
	t.mu.Unlock()

	if flushActivity {
		if err := saveUserActivityMap(t.dataDir, activitySnapshot); err != nil {
			log.Printf("failed to save user_activity.json: %v", err)
		} else {
			t.mu.Lock()
			t.dirtyActivity = false
			t.mu.Unlock()
		}
	}

	for _, p := range payloads {
		if err := t.writeFileAtomic(p.name, p.data); err != nil {
			log.Printf("failed to save %s: %v", p.name, err)
		}
	}
}

func (t *Tracker) recentEventsGCWorker() {
	ticker := time.NewTicker(t.recentGCInterval)
	defer ticker.Stop()

	for {
		select {
		case <-t.ctx.Done():
			return
		case <-ticker.C:
			t.cleanupRecentEvents(time.Now().UTC())
		}
	}
}

func (t *Tracker) cleanupRecentEvents(now time.Time) {
	cutoff := now.Add(-newUserNotifyWindow)
	t.mu.Lock()
	pruneRecentEventStore(t.recentVandalEvents, cutoff)
	pruneRecentEventStore(t.recentFixEvents, cutoff)
	t.mu.Unlock()
}

func (t *Tracker) activityGCWorker() {
	ticker := time.NewTicker(t.activityGCInterval)
	defer ticker.Stop()

	for {
		select {
		case <-t.ctx.Done():
			return
		case <-ticker.C:
			t.cleanupActivity(time.Now().UTC())
		}
	}
}

func (t *Tracker) cleanupActivity(now time.Time) {
	cutoff := now.AddDate(0, 0, -activityRetentionDays)
	t.mu.Lock()
	removedCount := 0
	for id, entry := range t.activity {
		if entry.LastSeen == "" {
			continue
		}
		lastSeen, err := time.Parse(time.RFC3339Nano, entry.LastSeen)
		if err != nil {
			// fallback to RFC3339
			lastSeen, err = time.Parse(time.RFC3339, entry.LastSeen)
		}
		if err == nil && lastSeen.Before(cutoff) {
			delete(t.activity, id)
			removedCount++
		}
	}
	if removedCount > 0 {
		t.dirtyActivity = true
		log.Printf("Activity GC: removed %d inactive users (cutoff=%s)", removedCount, cutoff.Format("2006-01-02"))
	}
	t.mu.Unlock()

	// Persist GC result immediately so evicted users are not restored on restart.
	if removedCount > 0 {
		t.flushDirtyState()
	}
}

func (t *Tracker) writeFileAtomic(filename string, payload []byte) error {
	if t.dataDir == "" {
		return fmt.Errorf("dataDir is empty")
	}
	path := filepath.Join(t.dataDir, filename)
	return utils.WriteFileAtomic(path, payload)
}

func ensureActivityMaps(entry *UserActivity) {
	if entry.DailyVandalCounts == nil {
		entry.DailyVandalCounts = make(map[string]int)
	}
	if entry.DailyRestoredCounts == nil {
		entry.DailyRestoredCounts = make(map[string]int)
	}
	if entry.DailyActivityScores == nil {
		entry.DailyActivityScores = make(map[string]int)
	}
}

func cloneUserActivity(src *UserActivity) UserActivity {
	if src == nil {
		return UserActivity{}
	}

	dst := *src
	dst.DailyVandalCounts = cloneStringIntMap(src.DailyVandalCounts)
	dst.DailyRestoredCounts = cloneStringIntMap(src.DailyRestoredCounts)
	dst.DailyActivityScores = cloneStringIntMap(src.DailyActivityScores)
	if src.LastPixel != nil {
		lastPixel := *src.LastPixel
		dst.LastPixel = &lastPixel
	}
	return dst
}

func cloneStringIntMap(src map[string]int) map[string]int {
	if src == nil {
		return nil
	}
	dst := make(map[string]int, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func buildDailyActivityScores(entry *UserActivity) map[string]int {
	out := make(map[string]int)
	for dateKey, count := range entry.DailyVandalCounts {
		out[dateKey] -= count
	}
	for dateKey, count := range entry.DailyRestoredCounts {
		out[dateKey] += count
	}
	return out
}

func diffPixelsToList(diff map[string]Pixel) [][]int {
	out := make([][]int, 0, len(diff))
	keys := make([]string, 0, len(diff))
	for key := range diff {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		px := diff[key]
		out = append(out, []int{px.AbsX, px.AbsY})
	}
	return out
}

func (t *Tracker) enqueuePixel(px Pixel) {
	key := pixelKey(px.AbsX, px.AbsY)
	t.mu.Lock()
	if _, exists := t.pending[key]; exists {
		t.mu.Unlock()
		return
	}
	t.pending[key] = px
	t.mu.Unlock()

	select {
	case <-t.ctx.Done():
		t.mu.Lock()
		delete(t.pending, key)
		t.mu.Unlock()
	case t.queue <- px:
	}
}

func countDiffChanges(oldDiff, newDiff map[string]Pixel) (added, removed int) {
	for key := range newDiff {
		if _, ok := oldDiff[key]; !ok {
			added++
		}
	}
	for key := range oldDiff {
		if _, ok := newDiff[key]; !ok {
			removed++
		}
	}
	return added, removed
}

func dateKeyJST() string {
	jst := time.FixedZone("JST", 9*3600)
	return time.Now().In(jst).Format("2006-01-02")
}

func pixelKey(x, y int) string {
	// strconv.AppendInt を使い fmt.Sprintf のフォーマット解析オーバーヘッドを回避。
	// 形式は "(x, y)" のまま維持し、既存の vandalized_pixels.json との互換性を保つ。
	buf := make([]byte, 0, 24)
	buf = append(buf, '(')
	buf = strconv.AppendInt(buf, int64(x), 10)
	buf = append(buf, ',', ' ')
	buf = strconv.AppendInt(buf, int64(y), 10)
	buf = append(buf, ')')
	return string(buf)
}

func recordRecentEvent(
	store map[string][]time.Time,
	userID string,
	now time.Time,
	window time.Duration,
) int {
	events := append(store[userID], now)
	cutoff := now.Add(-window)
	write := 0
	for _, ts := range events {
		if ts.Before(cutoff) {
			continue
		}
		events[write] = ts
		write++
	}
	events = events[:write]
	store[userID] = events
	return len(events)
}

func recordRecentEvents(
	store map[string][]time.Time,
	userID string,
	now time.Time,
	window time.Duration,
	count int,
) int {
	if count <= 0 {
		events := store[userID]
		cutoff := now.Add(-window)
		write := 0
		for _, ts := range events {
			if ts.Before(cutoff) {
				continue
			}
			events[write] = ts
			write++
		}
		events = events[:write]
		store[userID] = events
		return len(events)
	}
	// Notification threshold is small; cap synthetic events to keep memory bounded.
	capped := count
	if capped > newUserNotifyThreshold {
		capped = newUserNotifyThreshold
	}
	windowCount := 0
	for i := 0; i < capped; i++ {
		windowCount = recordRecentEvent(store, userID, now, window)
	}
	return windowCount
}

func chooseInferenceProbePixel(currentDiff map[string]Pixel, addedPixels []Pixel, pending map[string]Pixel, baseline map[string]Pixel) (Pixel, bool) {
	for _, px := range addedPixels {
		key := pixelKey(px.AbsX, px.AbsY)
		if _, inBaseline := baseline[key]; inBaseline {
			continue
		}
		if _, exists := pending[key]; !exists {
			return px, true
		}
	}
	for key, px := range currentDiff {
		if _, inBaseline := baseline[key]; inBaseline {
			continue
		}
		if _, exists := pending[key]; !exists {
			return px, true
		}
	}
	return Pixel{}, false
}

func chooseRestoreInferenceProbePixel(currentDiff map[string]Pixel, removedPixels []Pixel, pending map[string]Pixel, baseline map[string]Pixel) (Pixel, bool) {
	for _, px := range removedPixels {
		key := pixelKey(px.AbsX, px.AbsY)
		if _, exists := pending[key]; !exists {
			return px, true
		}
	}
	for key, px := range baseline {
		if _, stillDiff := currentDiff[key]; stillDiff {
			continue
		}
		if _, exists := pending[key]; !exists {
			return px, true
		}
	}
	return Pixel{}, false
}

func claimCurrentDiffPixels(currentDiff map[string]Pixel, painterID string, vandalState *VandalState, baseline map[string]Pixel) int {
	if len(currentDiff) == 0 || painterID == "" || vandalState == nil {
		return 0
	}
	count := 0
	for key := range currentDiff {
		if _, inBaseline := baseline[key]; inBaseline {
			continue
		}
		vandalState.PixelToPainter[key] = painterID
		count++
	}
	return count
}

func countRemovedFromBaseline(currentDiff map[string]Pixel, baseline map[string]Pixel) int {
	if len(baseline) == 0 {
		return 0
	}
	count := 0
	for key := range baseline {
		if _, stillDiff := currentDiff[key]; !stillDiff {
			count++
		}
	}
	return count
}

func armPowerSaveInference(state *powerSaveInferenceState, diffPixels int, now time.Time) bool {
	if state == nil {
		return false
	}
	if diffPixels < powerSaveInferenceMinPixels {
		resetPowerSaveInference(state)
		return false
	}
	state.Active = true
	state.ProbeQueued = false
	state.ClaimedPainter = ""
	state.RemainingPixels = diffPixels
	state.BaselinePixels = 0
	state.ExpiresAt = now.Add(powerSaveInferenceTTL)
	state.Baseline = nil
	return true
}

func armPowerSaveInferenceFromGrowth(state *powerSaveInferenceState, oldDiff map[string]Pixel, addedPixels int, now time.Time) bool {
	if !armPowerSaveInference(state, addedPixels, now) {
		return false
	}
	state.BaselinePixels = len(oldDiff)
	state.Baseline = copyPixelMap(oldDiff)
	return true
}

func armRestoreInferenceFromShrink(state *powerSaveInferenceState, oldDiff map[string]Pixel, removedPixels int, now time.Time) bool {
	if !armPowerSaveInference(state, removedPixels, now) {
		return false
	}
	state.BaselinePixels = len(oldDiff)
	state.Baseline = copyPixelMap(oldDiff)
	return true
}

func shouldAutoArmPowerSaveInference(added, removed int) bool {
	return removed == 0 && added >= powerSaveInferenceMinPixels
}

func shouldAutoArmRestoreInference(added, removed int) bool {
	return added == 0 && removed >= powerSaveInferenceMinPixels
}

func updatePowerSaveInferenceRemaining(state *powerSaveInferenceState, currentDiff map[string]Pixel) {
	if state == nil {
		return
	}
	remaining := len(currentDiff) - state.BaselinePixels
	if remaining < 0 {
		remaining = 0
	}
	state.RemainingPixels = remaining
}

func updateRestoreInferenceRemaining(state *powerSaveInferenceState, currentDiff map[string]Pixel) {
	if state == nil {
		return
	}
	remaining := state.BaselinePixels - len(currentDiff)
	if remaining < 0 {
		remaining = 0
	}
	state.RemainingPixels = remaining
}

func beginPowerSaveInference(state *powerSaveInferenceState, detectedPainterID string, now time.Time) (active bool, effectivePainterID string, creditPixels int) {
	if state == nil || !state.Active {
		return false, "", 0
	}
	if state.RemainingPixels <= 0 || now.After(state.ExpiresAt) {
		resetPowerSaveInference(state)
		return false, "", 0
	}
	if state.ClaimedPainter == "" {
		state.ClaimedPainter = detectedPainterID
		creditPixels = state.RemainingPixels
	}
	return true, state.ClaimedPainter, creditPixels
}

func consumePowerSaveInference(state *powerSaveInferenceState) {
	if state == nil || !state.Active {
		return
	}
	state.RemainingPixels--
	if state.RemainingPixels <= 0 {
		resetPowerSaveInference(state)
	}
}

func resetPowerSaveInference(state *powerSaveInferenceState) {
	if state == nil {
		return
	}
	state.Active = false
	state.ProbeQueued = false
	state.ClaimedPainter = ""
	state.RemainingPixels = 0
	state.BaselinePixels = 0
	state.ExpiresAt = time.Time{}
	state.Baseline = nil
}

func copyPixelMap(src map[string]Pixel) map[string]Pixel {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]Pixel, len(src))
	for key, px := range src {
		out[key] = px
	}
	return out
}

func clearInferenceProbeOnFetchFailure(vandalInference, restoreInference *powerSaveInferenceState) {
	if vandalInference != nil && vandalInference.Active && vandalInference.ClaimedPainter == "" && vandalInference.ProbeQueued {
		vandalInference.ProbeQueued = false
	}
	if restoreInference != nil && restoreInference.Active && restoreInference.ClaimedPainter == "" && restoreInference.ProbeQueued {
		restoreInference.ProbeQueued = false
	}
}

func pruneRecentEventStore(store map[string][]time.Time, cutoff time.Time) {
	for userID, events := range store {
		write := 0
		for _, ts := range events {
			if ts.Before(cutoff) {
				continue
			}
			events[write] = ts
			write++
		}
		if write == 0 {
			delete(store, userID)
			continue
		}
		store[userID] = events[:write]
	}
}

func loadDurationFromEnv(
	envKey string,
	defaultValue time.Duration,
	minValue time.Duration,
	maxValue time.Duration,
) time.Duration {
	raw := os.Getenv(envKey)
	if raw == "" {
		return defaultValue
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("invalid %s=%q: %v", envKey, raw, err)
		return defaultValue
	}
	d := time.Duration(seconds) * time.Second
	if d < minValue {
		return minValue
	}
	if d > maxValue {
		return maxValue
	}
	return d
}
