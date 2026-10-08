package monitor

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"Koukyo_discord_bot/internal/activity"
	"Koukyo_discord_bot/internal/utils"
	"Koukyo_discord_bot/internal/wplace"
)

const (
	// captureInterval タイルを取得して差分を計算する間隔
	captureInterval = 5 * time.Second
	// captureMaxBackoff 取得失敗が続いたときの最大待機時間
	captureMaxBackoff = 1 * time.Minute
	// captureTimeout 1回の取得にかけられる最大時間
	captureTimeout = 25 * time.Second
	// captureStaleAfter この時間を超えて取得に成功していなければ異常とみなす
	captureStaleAfter = 1 * time.Minute

	templateImageDirName = "template_img"
)

var monitorDebugLogging = os.Getenv("MONITOR_DEBUG_LOG") == "1"

func monitorDebugf(format string, args ...interface{}) {
	if !monitorDebugLogging {
		return
	}
	log.Printf(format, args...)
}

// Monitor wplaceのタイルを定期取得し、テンプレートとの差分を監視する
type Monitor struct {
	State        *MonitorState
	ctx          context.Context
	cancel       context.CancelFunc
	templatePath string
	ref          *reference
	mu           sync.RWMutex
	tracker      *activity.Tracker
	lastCaptured time.Time
}

// NewMonitor 新しいMonitorを作成
func NewMonitor(dataDir string) *Monitor {
	ctx, cancel := context.WithCancel(context.Background())
	base := fmt.Sprintf("%d-%d-%d-%d", utils.MainMonitorTileX, utils.MainMonitorTileY, utils.MainMonitorPixelX, utils.MainMonitorPixelY)
	return &Monitor{
		State:        NewMonitorState(),
		ctx:          ctx,
		cancel:       cancel,
		templatePath: filepath.Join(dataDir, templateImageDirName, base+".png"),
	}
}

func (m *Monitor) SetActivityTracker(tracker *activity.Tracker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tracker = tracker
}

// EnqueueDiffImageToTracker forwards a diff PNG to the activity tracker.
// Skipped when tracker is nil.
func (m *Monitor) EnqueueDiffImageToTracker(diffPNG []byte) {
	if m == nil || len(diffPNG) == 0 {
		return
	}
	m.mu.RLock()
	tracker := m.tracker
	m.mu.RUnlock()
	if tracker == nil {
		return
	}
	tracker.EnqueueDiffImage(diffPNG)
}

func (m *Monitor) GetCurrentDiffPainterCounts(limit int) []activity.PainterPixelCount {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	tracker := m.tracker
	m.mu.RUnlock()
	if tracker == nil {
		return nil
	}
	return tracker.GetCurrentDiffPainterCounts(limit)
}

// Start 監視を開始
func (m *Monitor) Start() {
	go m.runLoop("captureLoop", m.captureLoop)
}

func (m *Monitor) runLoop(name string, fn func()) {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("PANIC in %s: %v", name, r)
				}
			}()
			fn()
		}()

		select {
		case <-m.ctx.Done():
			return
		default:
		}
		log.Printf("%s stopped unexpectedly; restarting in 1s", name)
		time.Sleep(1 * time.Second)
	}
}

// captureLoop 定期的にタイルを取得して監視状態を更新する
func (m *Monitor) captureLoop() {
	failures := 0
	for {
		delay := captureInterval
		if err := m.captureOnce(); err != nil {
			if m.ctx.Err() != nil {
				return
			}
			// 失敗が続く間は指数バックオフで間隔を空ける。
			delay = captureInterval << uint(failures)
			if delay > captureMaxBackoff {
				delay = captureMaxBackoff
			}
			if failures < 4 {
				failures++
			}
			log.Printf("Monitor capture failed (retry in %v): %v", delay, err)
		} else {
			failures = 0
		}

		select {
		case <-m.ctx.Done():
			log.Println("Monitor stopped")
			return
		case <-time.After(delay):
		}
	}
}

// captureOnce タイルを取得してテンプレートと比較し、結果を State とトラッカーへ反映する
func (m *Monitor) captureOnce() error {
	ref, err := m.reference()
	if err != nil {
		return err
	}
	live, err := m.fetchLiveImage(ref.img.Bounds().Dx(), ref.img.Bounds().Dy())
	if err != nil {
		return err
	}
	result, err := ref.compare(live)
	if err != nil {
		return err
	}
	livePNG, err := encodePNG(result.live)
	if err != nil {
		return err
	}
	diffPNG, err := encodePNG(result.diff)
	if err != nil {
		return err
	}

	now := time.Now()
	m.State.UpdateData(result.data)
	m.State.UpdateImages(&ImageData{
		LiveImage: livePNG,
		DiffImage: diffPNG,
		Timestamp: now,
	})
	m.EnqueueDiffImageToTracker(diffPNG)

	m.mu.Lock()
	m.lastCaptured = now
	m.mu.Unlock()
	monitorDebugf("Updated: Diff=%.2f%% (%dpx)", result.data.DiffPercentage, result.data.DiffPixels)
	return nil
}

// reference はテンプレートを初回利用時に読み込む。読み込みに失敗した場合は次回再試行する。
func (m *Monitor) reference() (*reference, error) {
	m.mu.RLock()
	ref := m.ref
	m.mu.RUnlock()
	if ref != nil {
		return ref, nil
	}
	ref, err := loadReference(m.templatePath)
	if err != nil {
		return nil, err
	}
	log.Printf("Monitor template loaded: %s size=%dx%d", m.templatePath, ref.img.Bounds().Dx(), ref.img.Bounds().Dy())
	m.mu.Lock()
	m.ref = ref
	m.mu.Unlock()
	return ref, nil
}

// fetchLiveImage 監視範囲を含むタイルを取得し、監視範囲だけを切り出す
func (m *Monitor) fetchLiveImage(width, height int) (*image.NRGBA, error) {
	endPixelX := utils.MainMonitorPixelX + width
	endPixelY := utils.MainMonitorPixelY + height
	tilesX := (endPixelX + utils.WplaceTileSize - 1) / utils.WplaceTileSize
	tilesY := (endPixelY + utils.WplaceTileSize - 1) / utils.WplaceTileSize

	ctx, cancel := context.WithTimeout(m.ctx, captureTimeout)
	defer cancel()
	tilesData, err := wplace.DownloadTilesGridNoCache(ctx, nil, utils.MainMonitorTileX, utils.MainMonitorTileY, tilesX, tilesY, 16)
	if err != nil {
		return nil, fmt.Errorf("tile download failed: %w", err)
	}
	cropRect := image.Rect(utils.MainMonitorPixelX, utils.MainMonitorPixelY, endPixelX, endPixelY)
	live, err := wplace.CombineTilesCroppedImage(tilesData, utils.WplaceTileSize, utils.WplaceTileSize, tilesX, tilesY, cropRect)
	if err != nil {
		return nil, fmt.Errorf("tile combine failed: %w", err)
	}
	return live, nil
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// GetLatestData 最新の監視データを取得
func (m *Monitor) GetLatestData() *MonitorData {
	return m.State.GetLatestData()
}

// GetLatestImages 最新の画像データを取得
func (m *Monitor) GetLatestImages() *ImageData {
	m.State.mu.RLock()
	defer m.State.mu.RUnlock()

	if m.State.LatestImages == nil {
		return nil
	}

	return &ImageData{
		LiveImage: append([]byte(nil), m.State.LatestImages.LiveImage...),
		DiffImage: append([]byte(nil), m.State.LatestImages.DiffImage...),
		Timestamp: m.State.LatestImages.Timestamp,
	}
}

// GetLatestDiffImage returns a copy of the latest diff image only.
func (m *Monitor) GetLatestDiffImage() ([]byte, time.Time) {
	m.State.mu.RLock()
	defer m.State.mu.RUnlock()
	if m.State.LatestImages == nil || len(m.State.LatestImages.DiffImage) == 0 {
		return nil, time.Time{}
	}
	diffCopy := append([]byte(nil), m.State.LatestImages.DiffImage...)
	return diffCopy, m.State.LatestImages.Timestamp
}

// GetLatestDiffImageMeta returns timestamp and size for cache-keying without copying image bytes.
func (m *Monitor) GetLatestDiffImageMeta() (time.Time, int) {
	m.State.mu.RLock()
	defer m.State.mu.RUnlock()
	if m.State.LatestImages == nil {
		return time.Time{}, 0
	}
	return m.State.LatestImages.Timestamp, len(m.State.LatestImages.DiffImage)
}

// Stop 監視を停止
func (m *Monitor) Stop() {
	log.Println("Stopping monitor...")
	m.cancel()
	m.State.StopHeatmapWorker()
}

// IsHealthy 直近のタイル取得に成功しているかを返す
func (m *Monitor) IsHealthy() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.lastCaptured.IsZero() && time.Since(m.lastCaptured) < captureStaleAfter
}
