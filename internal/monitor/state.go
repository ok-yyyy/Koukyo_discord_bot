package monitor

import (
	"bytes"
	"container/ring"
	"context"
	"image/png"
	"math"
	"sort"
	"sync"
	"time"
)

const (
	// historyLimit 保持する差分履歴の最大数
	historyLimit = 20000
	// timelapseFrameLimit タイムラプスで保持するフレームの最大数
	timelapseFrameLimit = 512
	// zeroDiffEpsilon 0%判定の許容幅
	zeroDiffEpsilon = 0.005
)

// MonitorData WebSocketから受信する監視データ
type MonitorData struct {
	Type                     string    `json:"type"`
	Message                  string    `json:"message,omitempty"`
	DiffPercentage           float64   `json:"diff_percentage"`
	DiffPixels               int       `json:"diff_pixels"`
	WeightedDiffPercentage   *float64  `json:"weighted_diff_percentage"`
	WeightedDiffColor        string    `json:"weighted_diff_color,omitempty"`
	ChrysanthemumDiffPixels  int       `json:"chrysanthemum_diff_pixels"`
	BackgroundDiffPixels     int       `json:"background_diff_pixels"`
	ChrysanthemumTotalPixels int       `json:"chrysanthemum_total_pixels"`
	BackgroundTotalPixels    int       `json:"background_total_pixels"`
	TotalPixels              int       `json:"total_pixels"`
	Timestamp                time.Time `json:"-"`
}

// ImageData 画像データ
type ImageData struct {
	LiveImage []byte
	DiffImage []byte
	Timestamp time.Time
}

// MonitorState 現在の監視状態
type MonitorState struct {
	LatestData           *MonitorData
	LatestImages         *ImageData
	DiffHistory          *ring.Ring
	WeightedDiffHistory  *ring.Ring
	DiffHistoryCount     int
	WeightedHistoryCount int
	ReferencePixels      ReferencePixels
	PowerSaveMode        bool
	ZeroDiffStartTime    *time.Time
	// Timelapse recording
	TimelapseActive      bool
	TimelapseFrames      *ring.Ring
	LastTimelapseFrames  []TimelapseFrame
	TimelapseStartTime   *time.Time
	TimelapseCompletedAt *time.Time
	lastTimelapseCapture *time.Time
	// Heatmap aggregation (downsampled grid)
	HeatmapGridW   int
	HeatmapGridH   int
	HeatmapCounts  []uint32
	HeatmapSourceW int
	HeatmapSourceH int
	// Daily peak tracking (JST)
	DailyPeakDate      string
	DailyPeakDiff      float64
	DailyPeakAt        time.Time
	DailyPeakLiveImage []byte
	DailyPeakDiffImage []byte
	// Daily diff summary tracking (JST)
	DailySummaries    map[string]DailySummary
	heatmapQueue      chan []byte
	heatmapStopOnce   sync.Once
	heatmapCancelFunc context.CancelFunc
	mu                sync.RWMutex
}

// DiffRecord 差分履歴のレコード
type DiffRecord struct {
	Timestamp  time.Time
	Percentage float64
}

// DailyMetricSummary represents per-day aggregate values for one metric.
type DailyMetricSummary struct {
	Latest   float64
	LatestAt time.Time
	Max      float64
	Min      float64
	Sum      float64
	PeakAt   time.Time
	Count    int
}

// DailySummary represents per-day aggregates for overall/weighted diff.
type DailySummary struct {
	Overall  DailyMetricSummary
	Weighted DailyMetricSummary
}

// TimelapseFrame タイムラプスのフレーム（差分/ライブ画像を保持）
type TimelapseFrame struct {
	Timestamp time.Time
	DiffPNG   []byte
	LivePNG   []byte
}

// ReferencePixels 基準ピクセル数
type ReferencePixels struct {
	Total         int
	Chrysanthemum int
	Background    int
}

// NewMonitorState 新しい監視状態を作成
func NewMonitorState() *MonitorState {
	ctx, cancel := context.WithCancel(context.Background())
	ms := &MonitorState{
		DiffHistory:         ring.New(historyLimit),
		WeightedDiffHistory: ring.New(historyLimit),
		PowerSaveMode:       false,
		heatmapQueue:        make(chan []byte, 1),
		heatmapCancelFunc:   cancel,
		DailySummaries:      make(map[string]DailySummary),
	}
	ms.startHeatmapWorker(ctx)
	return ms
}

// StopHeatmapWorker ヒートマップワーカーを停止する（Monitor.Stop から呼ぶ）
func (ms *MonitorState) StopHeatmapWorker() {
	ms.heatmapStopOnce.Do(func() {
		ms.heatmapCancelFunc()
	})
}

// UpdateData 監視データを更新
func (ms *MonitorState) UpdateData(data *MonitorData) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	data.Timestamp = time.Now()
	ms.LatestData = data
	ms.updateDailySummaryLocked(data)

	// 基準ピクセル数の更新
	if data.ChrysanthemumTotalPixels > 0 {
		ms.ReferencePixels.Chrysanthemum = data.ChrysanthemumTotalPixels
	}
	if data.BackgroundTotalPixels > 0 {
		ms.ReferencePixels.Background = data.BackgroundTotalPixels
	}
	if data.TotalPixels > 0 {
		ms.ReferencePixels.Total = data.TotalPixels
	}

	// ゼロ差分の追跡
	if isZeroDiff(data.DiffPercentage) {
		if ms.ZeroDiffStartTime == nil {
			now := time.Now()
			ms.ZeroDiffStartTime = &now
		} else if !ms.PowerSaveMode {
			elapsed := time.Since(*ms.ZeroDiffStartTime)
			if elapsed >= 10*time.Minute {
				ms.PowerSaveMode = true
			}
		}
	} else {
		if ms.PowerSaveMode {
			ms.PowerSaveMode = false
		}
		ms.ZeroDiffStartTime = nil
	}

	// 差分履歴の追加（省電力中は保存しない）
	if !ms.PowerSaveMode {
		ms.DiffHistory.Value = DiffRecord{
			Timestamp:  data.Timestamp,
			Percentage: data.DiffPercentage,
		}
		ms.DiffHistory = ms.DiffHistory.Next()
		if ms.DiffHistoryCount < historyLimit {
			ms.DiffHistoryCount++
		}

		if data.WeightedDiffPercentage != nil {
			ms.WeightedDiffHistory.Value = DiffRecord{
				Timestamp:  data.Timestamp,
				Percentage: *data.WeightedDiffPercentage,
			}
			ms.WeightedDiffHistory = ms.WeightedDiffHistory.Next()
			if ms.WeightedHistoryCount < historyLimit {
				ms.WeightedHistoryCount++
			}
		}
	}

	// タイムラプスの開始/終了判定
	if !ms.TimelapseActive && data.DiffPercentage >= 30.0 {
		now := time.Now()
		ms.TimelapseActive = true
		ms.TimelapseFrames = ring.New(timelapseFrameLimit)
		ms.TimelapseStartTime = &now
		ms.TimelapseCompletedAt = nil
		ms.lastTimelapseCapture = nil
		if ms.LatestImages != nil && len(ms.LatestImages.DiffImage) > 0 {
			ms.addTimelapseFrameLocked(ms.LatestImages.LiveImage, ms.LatestImages.DiffImage, now)
		}
	}
	if ms.TimelapseActive && data.DiffPercentage <= 0.2 {
		now := time.Now()
		if ms.LatestImages != nil && len(ms.LatestImages.DiffImage) > 0 {
			if ms.lastTimelapseCapture == nil || now.Sub(*ms.lastTimelapseCapture) >= 10*time.Second {
				ms.addTimelapseFrameLocked(ms.LatestImages.LiveImage, ms.LatestImages.DiffImage, now)
			}
		}
		ms.TimelapseActive = false
		ms.LastTimelapseFrames = ms.collectTimelapseFramesLocked()
		ms.TimelapseCompletedAt = &now
		ms.TimelapseFrames = nil
		ms.TimelapseStartTime = nil
		ms.lastTimelapseCapture = nil
	}
}

// UpdateImages 画像データを更新
func (ms *MonitorState) UpdateImages(images *ImageData) {
	ms.mu.Lock()
	ms.LatestImages = images

	// Daily peak tracking (JST)
	if images != nil && len(images.DiffImage) > 0 && ms.LatestData != nil {
		jst := time.FixedZone("JST", 9*3600)
		ts := ms.LatestData.Timestamp
		if ts.IsZero() {
			ts = time.Now()
		}
		dateKey := ts.In(jst).Format("2006-01-02")
		if dateKey != ms.DailyPeakDate {
			ms.DailyPeakDate = dateKey
			ms.DailyPeakDiff = 0
			ms.DailyPeakAt = time.Time{}
			ms.DailyPeakLiveImage = nil
			ms.DailyPeakDiffImage = nil
		}
		if ms.DailyPeakDiffImage == nil || ms.LatestData.DiffPercentage >= ms.DailyPeakDiff {
			liveCopy := append([]byte(nil), images.LiveImage...)
			diffCopy := append([]byte(nil), images.DiffImage...)
			ms.DailyPeakLiveImage = liveCopy
			ms.DailyPeakDiffImage = diffCopy
			ms.DailyPeakDiff = ms.LatestData.DiffPercentage
			ms.DailyPeakAt = ts
		}
	}

	// タイムラプス中で、diff画像があり、一定間隔ごとにフレームを追加
	if ms.TimelapseActive && images != nil && len(images.DiffImage) > 0 {
		now := time.Now()
		if ms.lastTimelapseCapture == nil || now.Sub(*ms.lastTimelapseCapture) >= 10*time.Second {
			ms.addTimelapseFrameLocked(images.LiveImage, images.DiffImage, now)
		}
	}
	ms.mu.Unlock()

	// 省電力モードチェック
	ms.mu.RLock()
	isPowerSave := ms.PowerSaveMode
	ms.mu.RUnlock()
	if isPowerSave {
		return
	}

	// Heatmap集計はワーカーで最新のみ処理
	if images != nil && len(images.DiffImage) > 0 {
		diffImageCopy := make([]byte, len(images.DiffImage))
		copy(diffImageCopy, images.DiffImage)
		ms.enqueueHeatmap(diffImageCopy)
	}
}

func (ms *MonitorState) startHeatmapWorker(ctx context.Context) {
	go func() {
		for {
			select {
			case diff := <-ms.heatmapQueue:
				if diff != nil {
					ms.updateHeatmap(diff) //nolint:errcheck
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (ms *MonitorState) enqueueHeatmap(diffImage []byte) {
	select {
	case ms.heatmapQueue <- diffImage:
	default:
		select {
		case <-ms.heatmapQueue:
		default:
		}
		select {
		case ms.heatmapQueue <- diffImage:
		default:
		}
	}
}

// UpdateHeatmapFromDiff はヒートマップ集計を同期で行う
func (ms *MonitorState) UpdateHeatmapFromDiff(diffImage []byte) error {
	return ms.updateHeatmap(diffImage)
}

func (ms *MonitorState) updateHeatmap(diffImage []byte) error {
	img, err := png.Decode(bytes.NewReader(diffImage))
	if err != nil {
		// 必要であればログ出力
		// log.Printf("failed to decode diff image for heatmap: %v", err)
		return err
	}

	b := img.Bounds()
	w, h := b.Dx(), b.Dy()

	ms.mu.Lock()
	defer ms.mu.Unlock()

	if ms.HeatmapGridW == 0 || ms.HeatmapGridW*ms.HeatmapGridH == 0 || w != ms.HeatmapSourceW || h != ms.HeatmapSourceH {
		gridW := w
		gridH := h
		if gridW < 1 {
			gridW = 1
		}
		if gridH < 1 {
			gridH = 1
		}
		ms.HeatmapGridW = gridW
		ms.HeatmapGridH = gridH
		ms.HeatmapCounts = make([]uint32, gridW*gridH)
		ms.HeatmapSourceW = w
		ms.HeatmapSourceH = h
	}

	stepX := 1
	stepY := 1
	for y := b.Min.Y; y < b.Max.Y; y += stepY {
		for x := b.Min.X; x < b.Max.X; x += stepX {
			_, _, _, a := img.At(x, y).RGBA()
			if a > 0 {
				gx := int(float64(x-b.Min.X) * float64(ms.HeatmapGridW) / float64(w))
				gy := int(float64(y-b.Min.Y) * float64(ms.HeatmapGridH) / float64(h))
				if gx >= 0 && gx < ms.HeatmapGridW && gy >= 0 && gy < ms.HeatmapGridH {
					ms.HeatmapCounts[gy*ms.HeatmapGridW+gx]++
				}
			}
		}
	}
	return nil
}

// GetLatestDiffPercentage 最新の差分率を取得
func (ms *MonitorState) GetLatestDiffPercentage() float64 {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	if ms.LatestData != nil {
		return ms.LatestData.DiffPercentage
	}
	return 0
}

// HasData データを受信済みか
func (ms *MonitorState) HasData() bool {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.LatestData != nil
}

// GetLatestData 最新データのコピーを取得
func (ms *MonitorState) GetLatestData() *MonitorData {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	if ms.LatestData == nil {
		return nil
	}
	data := *ms.LatestData
	return &data
}

// IsPowerSaveMode reports whether power-save mode is enabled.
func (ms *MonitorState) IsPowerSaveMode() bool {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.PowerSaveMode
}

// SetPowerSaveMode updates the power-save mode flag.
func (ms *MonitorState) SetPowerSaveMode(enabled bool) {
	ms.mu.Lock()
	ms.PowerSaveMode = enabled
	ms.mu.Unlock()
}

// GetTimelapseCompletedAt returns a copy of the last timelapse completion time.
func (ms *MonitorState) GetTimelapseCompletedAt() *time.Time {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	if ms.TimelapseCompletedAt == nil {
		return nil
	}
	t := *ms.TimelapseCompletedAt
	return &t
}

// GetDiffHistory 期間内の差分履歴を取得
func (ms *MonitorState) GetDiffHistory(duration time.Duration, weighted bool) []DiffRecord {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	var src *ring.Ring
	if weighted {
		src = ms.WeightedDiffHistory
	} else {
		src = ms.DiffHistory
	}

	out := make([]DiffRecord, 0, src.Len())
	cutoff := time.Now().Add(-duration)

	src.Do(func(p interface{}) {
		if p == nil {
			return
		}
		r := p.(DiffRecord)
		if r.Timestamp.IsZero() {
			// Legacy/invalid records must be ignored; zero timestamps distort graph time axis.
			return
		}
		if duration <= 0 || r.Timestamp.After(cutoff) || r.Timestamp.Equal(cutoff) {
			out = append(out, r)
		}
	})

	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.Before(out[j].Timestamp)
	})

	return out
}

// GetDiffHistoryCount returns the number of stored diff records (capped at historyLimit).
func (ms *MonitorState) GetDiffHistoryCount() int {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.DiffHistoryCount
}

// collectTimelapseFramesLocked assumes ms.mu is already locked.
func (ms *MonitorState) collectTimelapseFramesLocked() []TimelapseFrame {
	if ms.TimelapseFrames == nil {
		return nil
	}
	out := make([]TimelapseFrame, 0, ms.TimelapseFrames.Len())
	ms.TimelapseFrames.Do(func(p interface{}) {
		if p != nil {
			frame := p.(TimelapseFrame)
			if !frame.Timestamp.IsZero() {
				out = append(out, frame)
			}
		}
	})
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.Before(out[j].Timestamp)
	})
	return out
}

func isZeroDiff(value float64) bool {
	return math.Abs(value) <= zeroDiffEpsilon
}

func (ms *MonitorState) resetDiffHistoryLocked() {
	ms.DiffHistory = ring.New(historyLimit)
	ms.WeightedDiffHistory = ring.New(historyLimit)
	ms.DiffHistoryCount = 0
	ms.WeightedHistoryCount = 0
}

// addTimelapseFrameLocked assumes ms.mu is already locked.
func (ms *MonitorState) addTimelapseFrameLocked(liveImage, diffImage []byte, now time.Time) {
	if ms.TimelapseFrames == nil || len(diffImage) == 0 {
		return
	}
	diffCopy := append([]byte(nil), diffImage...)
	liveCopy := append([]byte(nil), liveImage...)
	ms.TimelapseFrames.Value = TimelapseFrame{
		Timestamp: now,
		DiffPNG:   diffCopy,
		LivePNG:   liveCopy,
	}
	ms.TimelapseFrames = ms.TimelapseFrames.Next()
	ms.lastTimelapseCapture = &now
}

// GetLastTimelapseFrames 直近完了したタイムラプスのフレームを取得
func (ms *MonitorState) GetLastTimelapseFrames() []TimelapseFrame {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	if len(ms.LastTimelapseFrames) == 0 {
		return nil
	}
	out := make([]TimelapseFrame, len(ms.LastTimelapseFrames))
	copy(out, ms.LastTimelapseFrames)
	return out
}

// GetHeatmapSnapshot 集計済みヒートマップのスナップショット
func (ms *MonitorState) GetHeatmapSnapshot() (counts []uint32, gridW, gridH, srcW, srcH int) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	if ms.HeatmapCounts == nil || ms.HeatmapGridW == 0 || ms.HeatmapGridH == 0 {
		return nil, 0, 0, 0, 0
	}
	cp := make([]uint32, len(ms.HeatmapCounts))
	copy(cp, ms.HeatmapCounts)
	return cp, ms.HeatmapGridW, ms.HeatmapGridH, ms.HeatmapSourceW, ms.HeatmapSourceH
}

// GetDailyPeakDiffImage returns the diff image captured at the daily peak (JST).
func (ms *MonitorState) GetDailyPeakDiffImage(dateKey string) (img []byte, peakAt time.Time, peakValue float64, ok bool) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	if ms.DailyPeakDate != dateKey || len(ms.DailyPeakDiffImage) == 0 {
		return nil, time.Time{}, 0, false
	}
	cp := append([]byte(nil), ms.DailyPeakDiffImage...)
	return cp, ms.DailyPeakAt, ms.DailyPeakDiff, true
}

// GetDailyPeakImages returns live+diff images captured at the daily peak (JST).
func (ms *MonitorState) GetDailyPeakImages(dateKey string) (live []byte, diff []byte, peakAt time.Time, peakValue float64, ok bool) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	if ms.DailyPeakDate != dateKey || len(ms.DailyPeakDiffImage) == 0 {
		return nil, nil, time.Time{}, 0, false
	}
	liveCopy := append([]byte(nil), ms.DailyPeakLiveImage...)
	diffCopy := append([]byte(nil), ms.DailyPeakDiffImage...)
	return liveCopy, diffCopy, ms.DailyPeakAt, ms.DailyPeakDiff, true
}

// GetDailySummary returns aggregate diff summary for the given JST date key.
func (ms *MonitorState) GetDailySummary(dateKey string) (summary DailySummary, ok bool) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	summary, ok = ms.DailySummaries[dateKey]
	return summary, ok
}

func (ms *MonitorState) updateDailySummaryLocked(data *MonitorData) {
	if data == nil || data.Timestamp.IsZero() {
		return
	}

	jst := time.FixedZone("JST", 9*3600)
	dateKey := data.Timestamp.In(jst).Format("2006-01-02")

	summary := ms.DailySummaries[dateKey]
	summary.Overall = updateDailyMetric(summary.Overall, data.Timestamp, data.DiffPercentage)
	if data.WeightedDiffPercentage != nil {
		summary.Weighted = updateDailyMetric(summary.Weighted, data.Timestamp, *data.WeightedDiffPercentage)
	}
	ms.DailySummaries[dateKey] = summary

	// Keep memory bounded: preserve only recent 7 JST days.
	cutoff := data.Timestamp.In(jst).AddDate(0, 0, -7)
	for key := range ms.DailySummaries {
		day, err := time.ParseInLocation("2006-01-02", key, jst)
		if err != nil || day.Before(cutoff) {
			delete(ms.DailySummaries, key)
		}
	}
}

func updateDailyMetric(metric DailyMetricSummary, ts time.Time, value float64) DailyMetricSummary {
	if metric.Count == 0 || ts.After(metric.LatestAt) {
		metric.Latest = value
		metric.LatestAt = ts
	}
	if metric.Count == 0 || value > metric.Max {
		metric.Max = value
		metric.PeakAt = ts
	}
	if metric.Count == 0 || value < metric.Min {
		metric.Min = value
	}
	metric.Sum += value
	metric.Count++
	return metric
}
