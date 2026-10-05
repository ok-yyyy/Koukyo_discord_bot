package monitor

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"Koukyo_discord_bot/internal/activity"

	"github.com/gorilla/websocket"
)

const monitorReadTimeout = 60 * time.Second

var monitorDebugLogging = os.Getenv("MONITOR_DEBUG_LOG") == "1"

func monitorDebugf(format string, args ...interface{}) {
	if !monitorDebugLogging {
		return
	}
	log.Printf(format, args...)
}

// Monitor WebSocket監視クライアント
type Monitor struct {
	URL                string
	State              *MonitorState
	conn               *websocket.Conn
	ctx                context.Context
	cancel             context.CancelFunc
	connected          bool
	mu                 sync.RWMutex
	writeMu            sync.Mutex
	reconnectMu        sync.Mutex
	lastIdleReconnect  time.Time
	tracker            *activity.Tracker
	lastMu             sync.Mutex
	lastMsgAt          time.Time
	wsUnavailableSince time.Time
	reconnectAttempts  int
	reconnectBackoff   time.Duration
	pollURL            string
	pollClient         *http.Client
	pollBaseInterval   time.Duration
	pollMu             sync.Mutex
	pollAttempts       int
	pollNextAttemptAt  time.Time
}

type monitorTextPayload struct {
	Type                     string   `json:"type"`
	Message                  string   `json:"message,omitempty"`
	DiffPercentage           *float64 `json:"diff_percentage"`
	DiffPixels               *int     `json:"diff_pixels"`
	WeightedDiffPercentage   *float64 `json:"weighted_diff_percentage"`
	WeightedDiffColor        string   `json:"weighted_diff_color,omitempty"`
	ChrysanthemumDiffPixels  *int     `json:"chrysanthemum_diff_pixels"`
	BackgroundDiffPixels     *int     `json:"background_diff_pixels"`
	ChrysanthemumTotalPixels *int     `json:"chrysanthemum_total_pixels"`
	BackgroundTotalPixels    *int     `json:"background_total_pixels"`
	TotalPixels              *int     `json:"total_pixels"`
}

func (p *monitorTextPayload) hasMonitoringData() bool {
	if p == nil {
		return false
	}
	return p.DiffPercentage != nil || p.DiffPixels != nil || p.Type == "metadata"
}

func (p *monitorTextPayload) toMonitorData() *MonitorData {
	if p == nil {
		return nil
	}

	data := &MonitorData{
		Type:              p.Type,
		Message:           p.Message,
		WeightedDiffColor: p.WeightedDiffColor,
	}
	if p.DiffPercentage != nil {
		data.DiffPercentage = *p.DiffPercentage
	}
	if p.DiffPixels != nil {
		data.DiffPixels = *p.DiffPixels
	}
	if p.WeightedDiffPercentage != nil {
		weighted := *p.WeightedDiffPercentage
		data.WeightedDiffPercentage = &weighted
	}
	if p.ChrysanthemumDiffPixels != nil {
		data.ChrysanthemumDiffPixels = *p.ChrysanthemumDiffPixels
	}
	if p.BackgroundDiffPixels != nil {
		data.BackgroundDiffPixels = *p.BackgroundDiffPixels
	}
	if p.ChrysanthemumTotalPixels != nil {
		data.ChrysanthemumTotalPixels = *p.ChrysanthemumTotalPixels
	}
	if p.BackgroundTotalPixels != nil {
		data.BackgroundTotalPixels = *p.BackgroundTotalPixels
	}
	if p.TotalPixels != nil {
		data.TotalPixels = *p.TotalPixels
	}
	return data
}

// NewMonitor 新しいMonitorを作成
func NewMonitor(url string) *Monitor {
	ctx, cancel := context.WithCancel(context.Background())
	pollURL := strings.TrimSpace(os.Getenv("MONITOR_POLL_URL"))
	forceStandalone := os.Getenv("MONITOR_FORCE_STANDALONE") == "1"
	if forceStandalone {
		log.Println("⚠️ MONITOR_FORCE_STANDALONE enabled: starting in standalone mode without WebSocket")
	}
	return &Monitor{
		URL:              url,
		State:            NewMonitorState(),
		ctx:              ctx,
		cancel:           cancel,
		reconnectBackoff: 2 * time.Second,
		pollURL:          pollURL,
		pollClient:       &http.Client{Timeout: 10 * time.Second},
		pollBaseInterval: 10 * time.Second,
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

// Connect WebSocketサーバーに接続
func (m *Monitor) Connect() error {
	monitorDebugf("Connecting to WebSocket: %s", m.URL)

	m.mu.Lock()
	if m.conn != nil {
		m.conn.Close()
		m.conn = nil
		m.connected = false
	}
	m.mu.Unlock()

	conn, _, err := websocket.DefaultDialer.Dial(m.URL, nil)
	if err != nil {
		return err
	}
	conn.SetReadDeadline(time.Now().Add(monitorReadTimeout))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(monitorReadTimeout))
		return nil
	})

	m.mu.Lock()
	m.conn = conn
	m.connected = true
	m.mu.Unlock()
	m.lastMu.Lock()
	m.lastMsgAt = time.Now()
	m.lastMu.Unlock()

	log.Println("WebSocket connected successfully")
	return nil
}

// Start 監視を開始
func (m *Monitor) Start() error {
	if err := m.Connect(); err != nil {
		log.Printf("Initial WebSocket connect failed: %v; starting in degraded mode", err)
		m.markWSUnavailable(time.Now())
	}

	go m.runLoop("receiveLoop", m.receiveLoop)
	go m.runLoop("pingLoop", m.pingLoop)
	go m.runLoop("keepaliveLoop", m.keepaliveLoop)
	go m.runLoop("idleWatchLoop", m.idleWatchLoop)
	if m.pollURL != "" {
		go m.runLoop("pollFallbackLoop", m.pollFallbackLoop)
	} else {
		log.Println("Monitor polling fallback disabled (MONITOR_POLL_URL is empty)")
	}
	return nil
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

// receiveLoop メッセージ受信ループ
func (m *Monitor) receiveLoop() {
	defer func() {
		m.mu.Lock()
		if m.conn != nil {
			m.conn.Close()
		}
		m.connected = false
		m.mu.Unlock()
		m.markWSUnavailable(time.Now())
	}()

	for {
		select {
		case <-m.ctx.Done():
			log.Println("Monitor stopped")
			return
		default:
			m.mu.RLock()
			conn := m.conn
			m.mu.RUnlock()

			if conn == nil {
				m.markWSUnavailable(time.Now())
				// Exponential backoff with a max interval of 5 minutes.
				attempt := m.reconnectAttempts
				if attempt > 8 {
					attempt = 8
				}
				backoffDelay := m.reconnectBackoff * time.Duration(1<<uint(attempt))
				if backoffDelay > 5*time.Minute {
					backoffDelay = 5 * time.Minute
				}
				log.Printf("WebSocket disconnected; retrying in %v", backoffDelay)

				select {
				case <-m.ctx.Done():
					return
				case <-time.After(backoffDelay):
				}

				if err := m.Connect(); err != nil {
					log.Printf("Reconnect failed: %v", err)
					if m.reconnectAttempts < 8 {
						m.reconnectAttempts++
					}
					continue
				}
				log.Println("Reconnected successfully")
				continue
			}

			conn.SetReadDeadline(time.Now().Add(monitorReadTimeout))
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				log.Printf("WebSocket read error: %v", err)
				m.mu.Lock()
				if m.conn == conn {
					conn.Close()
					m.conn = nil
					m.connected = false
				}
				m.mu.Unlock()
				m.lastMu.Lock()
				m.lastMsgAt = time.Time{}
				m.lastMu.Unlock()
				m.markWSUnavailable(time.Now())
				// Trigger reconnection on next iteration
				continue
			}

			// Reset only after successful message receive.
			m.reconnectAttempts = 0
			m.markWSHealthy(time.Now())

			m.lastMu.Lock()
			m.lastMsgAt = time.Now()
			m.lastMu.Unlock()

			if err := m.handleMessage(messageType, message); err != nil {
				log.Printf("Message handling error: %v", err)
			}
		}
	}
}

func (m *Monitor) pingLoop() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.mu.RLock()
			conn := m.conn
			m.mu.RUnlock()
			if conn == nil {
				continue
			}
			deadline := time.Now().Add(5 * time.Second)
			m.writeMu.Lock()
			err := conn.WriteControl(websocket.PingMessage, []byte("ping"), deadline)
			m.writeMu.Unlock()
			if err != nil {
				log.Printf("WebSocket ping error: %v", err)
				m.mu.Lock()
				if m.conn == conn {
					m.conn.Close()
					m.conn = nil
					m.connected = false
				}
				m.mu.Unlock()
			}
		}
	}
}

func (m *Monitor) keepaliveLoop() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.mu.RLock()
			conn := m.conn
			m.mu.RUnlock()
			if conn == nil {
				continue
			}
			m.writeMu.Lock()
			err := conn.WriteMessage(websocket.TextMessage, []byte("ping"))
			m.writeMu.Unlock()
			if err != nil {
				log.Printf("WebSocket keepalive error: %v", err)
			}
		}
	}
}

func (m *Monitor) idleWatchLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	monitorDebugf("WebSocket idle watcher started")

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			if !m.IsConnected() {
				continue
			}
			m.lastMu.Lock()
			last := m.lastMsgAt
			m.lastMu.Unlock()
			if last.IsZero() {
				monitorDebugf("WebSocket idle for 2s: no messages received (no last message)")
				continue
			}
			elapsed := time.Since(last)
			if elapsed >= 2*time.Second {
				monitorDebugf("WebSocket idle: no messages received for %s", elapsed.Round(time.Millisecond))
			}
			// Emergency recovery: if the websocket doesn't deliver any monitoring data
			// for a long time (even if pongs are flowing), force a reconnect.
			if elapsed >= 60*time.Second {
				m.forceReconnectIfIdle()
			}
		}
	}
}

func (m *Monitor) forceReconnectIfIdle() {
	// Ensure only one idle reconnect attempt runs at a time.
	m.reconnectMu.Lock()
	defer m.reconnectMu.Unlock()

	// Cooldown: avoid tight reconnect loops if the server is genuinely quiet.
	now := time.Now()
	if !m.lastIdleReconnect.IsZero() && now.Sub(m.lastIdleReconnect) < 60*time.Second {
		return
	}
	m.lastIdleReconnect = now

	m.mu.Lock()
	conn := m.conn
	// Mark disconnected so receiveLoop will reconnect via the conn==nil path quickly.
	m.conn = nil
	m.connected = false
	m.mu.Unlock()
	m.markWSUnavailable(now)

	if conn != nil {
		_ = conn.Close()
	}
	monitorDebugf("WebSocket idle >60s: forced reconnect triggered")
}

func (m *Monitor) markWSUnavailable(now time.Time) {
	m.lastMu.Lock()
	if m.wsUnavailableSince.IsZero() {
		m.wsUnavailableSince = now
	}
	m.lastMu.Unlock()
}

func (m *Monitor) markWSHealthy(now time.Time) {
	m.lastMu.Lock()
	m.wsUnavailableSince = time.Time{}
	m.lastMsgAt = now
	m.lastMu.Unlock()
}

func (m *Monitor) getWSUnavailableSince() time.Time {
	m.lastMu.Lock()
	defer m.lastMu.Unlock()
	return m.wsUnavailableSince
}

// IsWSUnavailableFor reports whether WebSocket has been unavailable for at least d.
func (m *Monitor) IsWSUnavailableFor(d time.Duration) bool {
	if d <= 0 {
		return false
	}
	since := m.getWSUnavailableSince()
	if since.IsZero() {
		return false
	}
	return time.Since(since) >= d
}

func (m *Monitor) shouldPollFallback(now time.Time) bool {
	since := m.getWSUnavailableSince()
	if since.IsZero() {
		m.resetPollBackoff()
		return false
	}
	return now.Sub(since) >= 1*time.Minute
}

func (m *Monitor) resetPollBackoff() {
	m.pollMu.Lock()
	m.pollAttempts = 0
	m.pollNextAttemptAt = time.Time{}
	m.pollMu.Unlock()
}

func (m *Monitor) nextPollDelayLocked() time.Duration {
	attempt := m.pollAttempts
	if attempt > 5 {
		attempt = 5
	}
	delay := m.pollBaseInterval * time.Duration(1<<uint(attempt))
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	return delay
}

func (m *Monitor) pollFallbackLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			if !m.shouldPollFallback(now) {
				continue
			}

			m.pollMu.Lock()
			if !m.pollNextAttemptAt.IsZero() && now.Before(m.pollNextAttemptAt) {
				m.pollMu.Unlock()
				continue
			}
			m.pollMu.Unlock()

			data, err := m.fetchPolledData()
			if err != nil {
				m.pollMu.Lock()
				delay := m.nextPollDelayLocked()
				m.pollNextAttemptAt = time.Now().Add(delay)
				if m.pollAttempts < 5 {
					m.pollAttempts++
				}
				m.pollMu.Unlock()
				log.Printf("Monitor poll fallback failed: %v", err)
				continue
			}

			m.State.UpdateData(data)
			m.pollMu.Lock()
			m.pollAttempts = 0
			m.pollNextAttemptAt = time.Now().Add(m.pollBaseInterval)
			m.pollMu.Unlock()
		}
	}
}

func (m *Monitor) fetchPolledData() (*MonitorData, error) {
	req, err := http.NewRequestWithContext(m.ctx, http.MethodGet, m.pollURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := m.pollClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain to allow connection reuse
		return nil, fmt.Errorf("poll status=%d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parsePolledMonitorData(body)
}

func parsePolledMonitorData(body []byte) (*MonitorData, error) {
	var direct MonitorData
	if err := json.Unmarshal(body, &direct); err == nil {
		if direct.Type != "" || direct.TotalPixels > 0 || direct.DiffPixels > 0 || direct.DiffPercentage != 0 {
			return &direct, nil
		}
	}

	var wrapped struct {
		Data *MonitorData `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return nil, err
	}
	if wrapped.Data == nil {
		return nil, fmt.Errorf("invalid poll payload")
	}
	return wrapped.Data, nil
}

// handleMessage メッセージを処理
func (m *Monitor) handleMessage(messageType int, message []byte) error {
	switch messageType {
	case websocket.TextMessage:
		return m.handleTextMessage(message)
	case websocket.BinaryMessage:
		return m.handleBinaryMessage(message)
	}
	return nil
}

// handleTextMessage JSONメッセージを処理
func (m *Monitor) handleTextMessage(message []byte) error {
	var payload monitorTextPayload
	if err := json.Unmarshal(message, &payload); err != nil {
		return err
	}

	// エラーメッセージの場合
	if payload.Type == "error" {
		if payload.Message != "" {
			log.Printf("Server error: %s", payload.Message)
		} else {
			log.Printf("Server error payload: %s", string(message))
		}
		return nil
	}

	if payload.hasMonitoringData() {
		data := payload.toMonitorData()
		m.State.UpdateData(data)
		monitorDebugf("Updated: Diff=%.2f%%, Weighted=%.2f%%",
			data.DiffPercentage,
			getWeightedValue(data.WeightedDiffPercentage))
	}

	return nil
}

// handleBinaryMessage バイナリメッセージ（画像）を処理
func (m *Monitor) handleBinaryMessage(message []byte) error {
	// ヘッダーサイズ: 5バイト (type_id: 1バイト + payload_size: 4バイト)
	headerSize := 5
	if len(message) < headerSize {
		log.Printf("Binary message too short: %d bytes", len(message))
		return nil
	}

	typeID := message[0]
	payloadLen := int(binary.LittleEndian.Uint32(message[1:5]))
	if payloadLen < 0 || headerSize+payloadLen > len(message) {
		log.Printf("Binary payload size mismatch: header=%d, total=%d", payloadLen, len(message))
		payloadLen = len(message) - headerSize
	}
	payload := message[headerSize : headerSize+payloadLen]

	monitorDebugf("Received binary data: %d bytes, type_id=%d, payload_size=%d", len(message), typeID, payloadLen)

	m.mu.RLock()
	tracker := m.tracker
	m.mu.RUnlock()

	// payloadのコピーを作成（元のバッファが上書きされるのを防ぐ）
	payloadCopy := make([]byte, len(payload))
	copy(payloadCopy, payload)

	var current ImageData
	m.State.mu.RLock()
	if m.State.LatestImages != nil {
		current = *m.State.LatestImages
	}
	m.State.mu.RUnlock()

	updated := false
	now := time.Now()
	switch typeID {
	case 2: // Live image
		// 先頭に余分な00バイトがある場合は削除
		if len(payloadCopy) > 0 && payloadCopy[0] == 0x00 {
			payloadCopy = payloadCopy[1:]
		}
		current.LiveImage = payloadCopy
		current.Timestamp = now
		updated = true
		// 最初の16バイトをログに出力してフォーマットを確認
		if len(payloadCopy) >= 16 {
			monitorDebugf("Stored live image: %d bytes, header: %X", len(payloadCopy), payloadCopy[:16])
		} else {
			monitorDebugf("Stored live image: %d bytes", len(payloadCopy))
		}
	case 3: // Diff image
		// 先頭に余分な00バイトがある場合は削除
		if len(payloadCopy) > 0 && payloadCopy[0] == 0x00 {
			payloadCopy = payloadCopy[1:]
		}
		current.DiffImage = payloadCopy
		current.Timestamp = now
		updated = true
		if tracker != nil {
			tracker.EnqueueDiffImage(payloadCopy)
		}
		// 最初の16バイトをログに出力してフォーマットを確認
		if len(payloadCopy) >= 16 {
			monitorDebugf("Stored diff image: %d bytes, header: %X", len(payloadCopy), payloadCopy[:16])
		} else {
			monitorDebugf("Stored diff image: %d bytes", len(payloadCopy))
		}
	default:
		log.Printf("Unknown binary type_id: %d", typeID)
	}
	if updated {
		imagesCopy := &ImageData{
			LiveImage: append([]byte(nil), current.LiveImage...),
			DiffImage: append([]byte(nil), current.DiffImage...),
			Timestamp: current.Timestamp,
		}
		m.State.UpdateImages(imagesCopy)
	}

	return nil
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

	m.mu.Lock()
	if m.conn != nil {
		m.conn.Close()
	}
	m.mu.Unlock()
}

// IsConnected 接続状態を確認
func (m *Monitor) IsConnected() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connected
}

// getWeightedValue ポインタからfloat64を取得
func getWeightedValue(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
