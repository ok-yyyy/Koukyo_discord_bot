package notifications

import (
	"Koukyo_discord_bot/internal/activity"
	"Koukyo_discord_bot/internal/config"
	"Koukyo_discord_bot/internal/embeds"
	"Koukyo_discord_bot/internal/monitor"
	"Koukyo_discord_bot/internal/utils"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

type dispatchFunc func()

// NotificationState サーバーごとの通知状態
type NotificationState struct {
	mu                        sync.Mutex
	LastTier                  Tier
	MentionTriggered          bool
	WasZeroDiff               bool // 前回が0%だったか
	SmallDiffMessageID        string
	SmallDiffMessageChannelID string
	SmallDiffActive           bool
	LargeDiffActive           bool
	SmallDiffLastContent      string
	SmallDiffNextUpdate       time.Time
}

// Notifier 通知システム
type Notifier struct {
	session                  *discordgo.Session
	monitor                  *monitor.Monitor
	settings                 *config.SettingsManager
	states                   map[string]*NotificationState
	mu                       sync.RWMutex
	lastTimelapseCompletedAt *time.Time
	lastPowerSaveMode        bool
	timelapsePostMu          sync.Mutex
	timelapsePosting         bool
	dispatchHigh             chan dispatchFunc
	dispatchLowMu            sync.Mutex
	dispatchLowPending       map[string]dispatchFunc
	dispatchLowQueued        map[string]bool
	dispatchLowQueue         chan string
	dataDir                  string
	lastDailyReportDate      string
	vandalUserNotifier       *VandalUserNotifier
	fixUserNotifier          *FixUserNotifier
	watchTargetsState        *watchTargetsRuntime
	progressTargetsState     *progressTargetsRuntime
	droppedHighPriority      uint64
	droppedLowPriority       uint64
	metricsMu                sync.Mutex
	wplaceHealth             wplaceHealthState
	standaloneMu             sync.Mutex
	standaloneNextRun        time.Time
	standaloneAttempts       int
	standaloneActive         bool
	standaloneStartedAt      time.Time
	standaloneErrorCount     int
	standaloneLastError      string
	standaloneLastErrorAt    time.Time
	standaloneLastErrorNotif time.Time
	smallDiffCacheMu         sync.Mutex
	smallDiffCacheTS         time.Time
	smallDiffCacheDiffLen    int
	smallDiffCacheLimit      int
	smallDiffCacheLines      []string
	achievementEvalMu        sync.Mutex
	achievementBaselineReady bool
	dmUserStatesMu           sync.Mutex
	dmUserStates             map[string]*dmUserState
}

// NewNotifier 通知システムを作成
func NewNotifier(session *discordgo.Session, mon *monitor.Monitor, settings *config.SettingsManager, dataDir string) *Notifier {
	return &Notifier{
		session:              session,
		monitor:              mon,
		settings:             settings,
		states:               make(map[string]*NotificationState),
		dispatchHigh:         make(chan dispatchFunc, 256),
		dispatchLowPending:   make(map[string]dispatchFunc),
		dispatchLowQueued:    make(map[string]bool),
		dispatchLowQueue:     make(chan string, 2048),
		dataDir:              dataDir,
		vandalUserNotifier:   NewVandalUserNotifier(session, settings),
		fixUserNotifier:      NewFixUserNotifier(session, settings),
		watchTargetsState:    newWatchTargetsRuntime(dataDir),
		progressTargetsState: newProgressTargetsRuntime(dataDir),
		dmUserStates:         make(map[string]*dmUserState),
	}
}

func (n *Notifier) startDispatchWorker() {
	if n == nil {
		return
	}
	go func() {
		for {
			// High priority (FIFO, no coalescing).
			select {
			case fn := <-n.dispatchHigh:
				if fn != nil {
					fn()
				}
				continue
			default:
			}

			// If no high-priority work is immediately available, block on either.
			select {
			case fn := <-n.dispatchHigh:
				if fn != nil {
					fn()
				}
			case key := <-n.dispatchLowQueue:
				n.dispatchLowMu.Lock()
				fn := n.dispatchLowPending[key]
				delete(n.dispatchLowPending, key)
				n.dispatchLowQueued[key] = false
				n.dispatchLowMu.Unlock()
				if fn != nil {
					fn()
				}
			}
		}
	}()
}

func (n *Notifier) EnqueueHigh(fn dispatchFunc) {
	if n == nil || fn == nil {
		return
	}
	select {
	case n.dispatchHigh <- fn:
	default:
		// Drop if overloaded; do not block the monitoring loop.
		n.metricsMu.Lock()
		n.droppedHighPriority++
		dropped := n.droppedHighPriority
		n.metricsMu.Unlock()
		log.Printf("⚠️ dispatch: high queue full, dropping notification (total dropped: %d)", dropped)
	}
}

func (n *Notifier) enqueueLow(key string, fn dispatchFunc) {
	if n == nil || fn == nil || key == "" {
		return
	}
	n.dispatchLowMu.Lock()
	n.dispatchLowPending[key] = fn
	if n.dispatchLowQueued[key] {
		n.dispatchLowMu.Unlock()
		return
	}
	n.dispatchLowQueued[key] = true
	n.dispatchLowMu.Unlock()

	select {
	case n.dispatchLowQueue <- key:
	default:
		// Queue full: mark as not queued so we can try again later; keep latest pending.
		n.dispatchLowMu.Lock()
		n.dispatchLowQueued[key] = false
		n.dispatchLowMu.Unlock()
		n.metricsMu.Lock()
		n.droppedLowPriority++
		dropped := n.droppedLowPriority
		n.metricsMu.Unlock()
		log.Printf("⚠️ dispatch: low queue full, dropping enqueue key=%s (total dropped: %d)", key, dropped)
	}
}

// GetDroppedNotificationStats 通知ドロップ統計を取得
func (n *Notifier) GetDroppedNotificationStats() (high, low uint64) {
	n.metricsMu.Lock()
	defer n.metricsMu.Unlock()
	return n.droppedHighPriority, n.droppedLowPriority
}

// getState サーバーの通知状態を取得
func (n *Notifier) getState(guildID string) *NotificationState {
	n.mu.Lock()
	defer n.mu.Unlock()

	if state, ok := n.states[guildID]; ok {
		return state
	}

	state := &NotificationState{
		LastTier:         TierNone,
		MentionTriggered: false,
		WasZeroDiff:      true, // 初回は0%とみなす
	}
	n.states[guildID] = state
	return state
}

// smallDiffPixelLimit is the max pixel count treated as "small diff" noise.
// While within this limit, we keep a single text message and edit it to reduce spam.
const smallDiffPixelLimit = 10

const smallDiffMinUpdateInterval = 5 * time.Second

const diffUserSummaryTopN = 5

func (n *Notifier) upsertSmallDiffMessage(channelID string, state *NotificationState, content string, force bool) {
	if n == nil || n.session == nil || state == nil || channelID == "" {
		return
	}
	now := time.Now()
	if !force {
		// Avoid hammering Discord with edits every tick; keep the loop responsive.
		state.mu.Lock()
		sameContent := content == state.SmallDiffLastContent
		nextUpdate := state.SmallDiffNextUpdate
		state.mu.Unlock()
		if sameContent && !nextUpdate.IsZero() && now.Before(nextUpdate) {
			return
		}
		if !nextUpdate.IsZero() && now.Before(nextUpdate) {
			return
		}
	}

	// Optimistic throttle: even if the dispatcher is backlogged, avoid enqueuing
	// edits too frequently.
	state.mu.Lock()
	state.SmallDiffLastContent = content
	state.SmallDiffNextUpdate = now.Add(smallDiffMinUpdateInterval)
	state.mu.Unlock()

	// Coalesce small-diff updates per guild+channel: keep only the latest edit.
	key := fmt.Sprintf("small:%s:%s", channelID, guildKeyFromState(state))
	n.enqueueLow(key, func() {
		state.mu.Lock()
		msgID := state.SmallDiffMessageID
		msgCh := state.SmallDiffMessageChannelID
		state.mu.Unlock()

		// Try edit first.
		if msgID != "" && msgCh == channelID {
			if _, err := n.session.ChannelMessageEdit(channelID, msgID, content); err == nil {
				return
			}
		}
		msg, err := n.session.ChannelMessageSend(channelID, content)
		if err != nil {
			log.Printf("Failed to send small-diff notification to channel %s: %v", channelID, err)
			return
		}
		state.mu.Lock()
		state.SmallDiffMessageID = msg.ID
		state.SmallDiffMessageChannelID = channelID
		state.mu.Unlock()
	})
}

func guildKeyFromState(state *NotificationState) string {
	// State objects are per guild in n.states; we just need a stable key for coalescing.
	// Pointer identity is stable within process lifetime.
	return fmt.Sprintf("%p", state)
}

// resetAllSmallDiffMessageTracking clears editable text-message pointers so the next
// small-diff event starts from a fresh message instead of editing an old one.
func (n *Notifier) resetAllSmallDiffMessageTracking() {
	if n == nil {
		return
	}

	n.mu.RLock()
	states := make([]*NotificationState, 0, len(n.states))
	for _, state := range n.states {
		states = append(states, state)
	}
	n.mu.RUnlock()

	for _, state := range states {
		if state == nil {
			continue
		}
		state.mu.Lock()
		state.SmallDiffMessageID = ""
		state.SmallDiffMessageChannelID = ""
		state.SmallDiffLastContent = ""
		state.SmallDiffNextUpdate = time.Time{}
		state.mu.Unlock()
	}
}

func (n *Notifier) buildSmallDiffCoordinateLines(limit int) []string {
	if n == nil || n.monitor == nil || limit <= 0 {
		return nil
	}
	ts, diffLen := n.monitor.GetLatestDiffImageMeta()
	if diffLen == 0 {
		return nil
	}

	n.smallDiffCacheMu.Lock()
	if n.smallDiffCacheLimit == limit &&
		n.smallDiffCacheDiffLen == diffLen &&
		!n.smallDiffCacheTS.IsZero() &&
		n.smallDiffCacheTS.Equal(ts) {
		lines := append([]string(nil), n.smallDiffCacheLines...)
		n.smallDiffCacheMu.Unlock()
		return lines
	}
	n.smallDiffCacheMu.Unlock()

	diffImage, imageTS := n.monitor.GetLatestDiffImage()
	if len(diffImage) == 0 {
		return nil
	}

	lines, err := smallDiffCoordinateLines(diffImage, limit)
	if err != nil {
		log.Printf("small_diff: failed to build coordinate lines: %v", err)
		return nil
	}

	n.smallDiffCacheMu.Lock()
	n.smallDiffCacheTS = imageTS
	n.smallDiffCacheDiffLen = len(diffImage)
	n.smallDiffCacheLimit = limit
	n.smallDiffCacheLines = append([]string(nil), lines...)
	n.smallDiffCacheMu.Unlock()

	return lines
}

// CheckAndNotify 差分率をチェックして通知を送信 (Refactored)
func (n *Notifier) CheckAndNotify(guildID string) {
	settings := n.settings.GetGuildSettings(guildID)

	// 自動通知が無効、または通知チャンネル未設定の場合はスキップ
	if !settings.AutoNotifyEnabled || settings.NotificationChannel == nil {
		return
	}

	// 監視データを取得
	data := n.monitor.GetLatestData()
	if data == nil || n.monitor.State.IsPowerSaveMode() {
		return
	}

	// 通知指標の値を取得
	diffValue := getDiffValue(data, settings.NotificationMetric)
	isZero := isZeroDiff(diffValue)
	currentTier := calculateTier(diffValue, settings.NotificationThreshold)
	state := n.getState(guildID)

	// 1. 小規模差分（Small Diff）の処理
	if n.handleSmallDiff(state, settings, data, diffValue, currentTier, isZero) {
		return
	}

	// 2. 大規模差分への遷移チェック
	n.handleLargeDiffTransition(guildID, state, settings, data, diffValue, isZero)

	// 3. 0%復帰・完了・Tier変動の標準通知処理
	n.handleStandardNotification(guildID, state, settings, data, diffValue, currentTier, isZero)

	// 4. 状態更新
	n.updateState(state, isZero, currentTier, diffValue, settings)
}

// handleSmallDiff 小規模差分の処理。trueを返した場合は後続処理をスキップする。
func (n *Notifier) handleSmallDiff(
	state *NotificationState,
	settings config.GuildSettings,
	data *monitor.MonitorData,
	diffValue float64,
	currentTier Tier,
	isZero bool,
) bool {
	if !state.LargeDiffActive && data.DiffPixels > 0 && data.DiffPixels <= smallDiffPixelLimit {
		metricLabel := "差分率"
		if settings.NotificationMetric == "weighted" {
			metricLabel = "加重差分率"
		}
		content := fmt.Sprintf(
			"🔔 【Wplace速報】変化検知 %s: **%.2f%%**に上昇(%d/%d px)",
			metricLabel,
			diffValue,
			data.DiffPixels,
			data.TotalPixels,
		)
		if lines := n.buildSmallDiffCoordinateLines(smallDiffPixelLimit); len(lines) > 0 {
			content += "\n" + strings.Join(lines, "\n")
		}
		n.upsertSmallDiffMessage(*settings.NotificationChannel, state, content, false)
		state.SmallDiffActive = true
		state.LastTier = currentTier
		state.MentionTriggered = diffValue >= settings.MentionThreshold
		state.WasZeroDiff = isZero
		return true
	}
	return false
}

// handleLargeDiffTransition 大規模差分モードへの遷移処理
func (n *Notifier) handleLargeDiffTransition(
	guildID string,
	state *NotificationState,
	settings config.GuildSettings,
	data *monitor.MonitorData,
	diffValue float64,
	isZero bool,
) {
	if !isZero && data.DiffPixels > smallDiffPixelLimit {
		transitionedFromSmall := state.SmallDiffActive && !state.LargeDiffActive
		state.LargeDiffActive = true
		state.SmallDiffActive = false
		state.mu.Lock()
		state.SmallDiffMessageID = ""
		state.SmallDiffMessageChannelID = ""
		state.SmallDiffLastContent = ""
		state.SmallDiffNextUpdate = time.Time{}
		state.mu.Unlock()

		if transitionedFromSmall {
			n.sendLargeDiffTransitionSnapshot(guildID, settings, data, diffValue)
		}
	}
}

// handleStandardNotification 通常の通知フロー
func (n *Notifier) handleStandardNotification(
	guildID string,
	state *NotificationState,
	settings config.GuildSettings,
	data *monitor.MonitorData,
	diffValue float64,
	currentTier Tier,
	isZero bool,
) {
	if state.WasZeroDiff && !isZero {
		n.sendZeroRecoveryNotification(guildID, settings, data, diffValue)
	}

	if !state.WasZeroDiff && isZero {
		if state.SmallDiffActive && !state.LargeDiffActive {
			metricLabel := "差分率"
			if settings.NotificationMetric == "weighted" {
				metricLabel = "加重差分率"
			}
			content := fmt.Sprintf("✅ 【Wplace速報】修復完了！ %s: 0.00%% # Pixel Perfect!", metricLabel)
			n.upsertSmallDiffMessage(*settings.NotificationChannel, state, content, true)
			return
		} else {
			n.sendZeroCompletionNotification(guildID, settings, data)
		}
	}

	if !isZero && currentTier != state.LastTier {
		if currentTier > state.LastTier {
			n.sendNotification(guildID, settings, data, currentTier, diffValue)
		} else {
			n.sendDecreaseNotification(guildID, settings, data, currentTier, diffValue)
		}
	}
}

// updateState 通知後の状態更新
func (n *Notifier) updateState(
	state *NotificationState,
	isZero bool,
	currentTier Tier,
	diffValue float64,
	settings config.GuildSettings,
) {
	if isZero {
		state.SmallDiffActive = false
		state.LargeDiffActive = false
	}
	state.LastTier = currentTier
	state.MentionTriggered = diffValue >= settings.MentionThreshold
	state.WasZeroDiff = isZero
}

// sendLargeDiffTransitionSnapshot posts an embed snapshot when we leave the small-diff thread.
func (n *Notifier) sendLargeDiffTransitionSnapshot(
	guildID string,
	settings config.GuildSettings,
	data *monitor.MonitorData,
	diffValue float64,
) {
	if n == nil || n.session == nil || settings.NotificationChannel == nil {
		return
	}
	channelID := *settings.NotificationChannel

	metricLabel := "差分率"
	if settings.NotificationMetric == "weighted" {
		metricLabel = "加重差分率"
	}

	message := fmt.Sprintf("🔔 【Wplace速報】変化検知 %s: **%.2f%%**に上昇(%d/%d px)", metricLabel, diffValue, data.DiffPixels, data.TotalPixels)

	embed := &discordgo.MessageEmbed{
		Title:       "🟡 Wplace 変化検知",
		Description: fmt.Sprintf("差分ピクセルが%dpxを超えました\n現在の%s: **%.2f%%**", smallDiffPixelLimit, metricLabel, diffValue),
		Color:       0xF1C40F, // gold-ish
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "📊 差分率 (全体)",
				Value:  fmt.Sprintf("%.2f%%", data.DiffPercentage),
				Inline: true,
			},
			{
				Name:   "📈 差分ピクセル (全体)",
				Value:  fmt.Sprintf("%d / %d", data.DiffPixels, data.TotalPixels),
				Inline: true,
			},
		},
		Timestamp: time.Now().Format(time.RFC3339),
		Footer: &discordgo.MessageEmbedFooter{
			Text: "自動通知システム",
		},
	}

	if data.WeightedDiffPercentage != nil {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "🔍 加重差分率 (菊重視)",
			Value:  fmt.Sprintf("%.2f%%", *data.WeightedDiffPercentage),
			Inline: true,
		})
	}
	if data.ChrysanthemumDiffPixels > 0 || data.BackgroundDiffPixels > 0 {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "🔍 差分ピクセル (菊/背景)",
			Value:  fmt.Sprintf("菊 %d / %d | 背景 %d / %d", data.ChrysanthemumDiffPixels, data.ChrysanthemumTotalPixels, data.BackgroundDiffPixels, data.BackgroundTotalPixels),
			Inline: false,
		})
	}
	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
		Name:   "📐 監視ピクセル数",
		Value:  fmt.Sprintf("全体 %d | 菊 %d | 背景 %d", data.TotalPixels, data.ChrysanthemumTotalPixels, data.BackgroundTotalPixels),
		Inline: false,
	})
	appendCurrentDiffUserSummaryField(n, embed)
	appendMainMonitorMapField(embed)

	var files []*discordgo.File
	images := n.monitor.GetLatestImages()
	if images != nil && images.LiveImage != nil && images.DiffImage != nil {
		combinedImage, err := embeds.CombineImages(images.LiveImage, images.DiffImage)
		if err == nil {
			files = append(files, &discordgo.File{
				Name:        "koukyo_status.png",
				ContentType: "image/png",
				Reader:      combinedImage,
			})
			embed.Image = &discordgo.MessageEmbedImage{
				URL: "attachment://koukyo_status.png",
			}
		} else {
			log.Printf("Failed to combine images for transition snapshot: %v", err)
		}
	}

	if _, err := n.session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: message,
		Embeds:  []*discordgo.MessageEmbed{embed},
		Files:   files,
	}); err != nil {
		log.Printf("Failed to send transition snapshot to channel %s: %v", channelID, err)
	} else {
		log.Printf("Transition snapshot sent to guild %s: %.2f%%", guildID, diffValue)
	}
}

// sendNotification 通知を送信
func (n *Notifier) sendNotification(
	guildID string,
	settings config.GuildSettings,
	data *monitor.MonitorData,
	tier Tier,
	diffValue float64,
) {
	channelID := *settings.NotificationChannel

	mentionStr := ""
	if diffValue >= settings.MentionThreshold && settings.MentionRole != nil {
		mentionStr = fmt.Sprintf("<@&%s> ", *settings.MentionRole)
	}

	metricLabel := "差分率"
	if settings.NotificationMetric == "weighted" {
		metricLabel = "加重差分率"
	}

	var tierDesc string
	switch tier {
	case Tier100:
		tierDesc = "100%に急増!!"
	case Tier90:
		tierDesc = "90%台に増加"
	case Tier80:
		tierDesc = "80%台に増加"
	case Tier70:
		tierDesc = "70%台に増加"
	case Tier60:
		tierDesc = "60%台に増加"
	case Tier50:
		tierDesc = "50%以上に急増"
	case Tier40:
		tierDesc = "40%台に増加"
	case Tier30:
		tierDesc = "30%台に増加"
	case Tier20:
		tierDesc = "20%台に増加"
	case Tier10:
		tierDesc = "10%台に増加"
	default:
		tierDesc = "変動"
	}

	message := fmt.Sprintf(
		"%s【Wplace速報】 🚨 %sが%sしました！[現在%.2f%%]",
		mentionStr,
		metricLabel,
		tierDesc,
		diffValue,
	)

	embed := &discordgo.MessageEmbed{
		Title:       "🏯 Wplace 荒らし検知",
		Description: fmt.Sprintf("現在の%s: **%.2f%%**", metricLabel, diffValue),
		Color:       getTierColor(tier),
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "📊 差分率 (全体)",
				Value:  fmt.Sprintf("%.2f%%", data.DiffPercentage),
				Inline: true,
			},
			{
				Name:   "📈 差分ピクセル (全体)",
				Value:  fmt.Sprintf("%d / %d", data.DiffPixels, data.TotalPixels),
				Inline: true,
			},
		},
		Timestamp: time.Now().Format(time.RFC3339),
		Footer: &discordgo.MessageEmbedFooter{
			Text: "自動通知システム",
		},
	}

	if data.WeightedDiffPercentage != nil {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "🔍 加重差分率 (菊重視)",
			Value:  fmt.Sprintf("%.2f%%", *data.WeightedDiffPercentage),
			Inline: true,
		})
	}

	if data.ChrysanthemumDiffPixels > 0 || data.BackgroundDiffPixels > 0 {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "🔍 差分ピクセル (菊/背景)",
			Value:  fmt.Sprintf("菊 %d / %d | 背景 %d / %d", data.ChrysanthemumDiffPixels, data.ChrysanthemumTotalPixels, data.BackgroundDiffPixels, data.BackgroundTotalPixels),
			Inline: false,
		})
	}

	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
		Name:   "📐 監視ピクセル数",
		Value:  fmt.Sprintf("全体 %d | 菊 %d | 背景 %d", data.TotalPixels, data.ChrysanthemumTotalPixels, data.BackgroundTotalPixels),
		Inline: false,
	})
	appendCurrentDiffUserSummaryField(n, embed)
	appendMainMonitorMapField(embed)

	var files []*discordgo.File
	images := n.monitor.GetLatestImages()
	if images != nil && images.LiveImage != nil && images.DiffImage != nil {
		combinedImage, err := embeds.CombineImages(images.LiveImage, images.DiffImage)
		if err == nil {
			files = append(files, &discordgo.File{
				Name:        "koukyo_status.png",
				ContentType: "image/png",
				Reader:      combinedImage,
			})
			embed.Image = &discordgo.MessageEmbedImage{
				URL: "attachment://koukyo_status.png",
			}
		} else {
			log.Printf("Failed to combine images for notification: %v", err)
		}
	}

	_, err := n.session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: message,
		Embeds:  []*discordgo.MessageEmbed{embed},
		Files:   files,
	})

	if err != nil {
		log.Printf("Failed to send notification to channel %s: %v", channelID, err)
	} else {
		log.Printf("Notification sent to guild %s: %.2f%%", guildID, diffValue)
	}
}

// sendDecreaseNotification Tierが下がった通知を送信
func (n *Notifier) sendDecreaseNotification(
	guildID string,
	settings config.GuildSettings,
	data *monitor.MonitorData,
	tier Tier,
	diffValue float64,
) {
	channelID := *settings.NotificationChannel

	metricLabel := "差分率"
	if settings.NotificationMetric == "weighted" {
		metricLabel = "加重差分率"
	}

	tierLabel := tierRangeLabel(tier, settings.NotificationThreshold)
	message := fmt.Sprintf(
		"【Wplace速報】 %sが%sまで減少しました。[現在%.2f%%]",
		metricLabel,
		tierLabel,
		diffValue,
	)

	embed := &discordgo.MessageEmbed{
		Title:       "🏯 Wplace 差分減少",
		Description: fmt.Sprintf("現在の%s: **%.2f%%**", metricLabel, diffValue),
		Color:       getTierColor(tier),
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "📊 差分率 (全体)",
				Value:  fmt.Sprintf("%.2f%%", data.DiffPercentage),
				Inline: true,
			},
			{
				Name:   "📈 差分ピクセル (全体)",
				Value:  fmt.Sprintf("%d / %d", data.DiffPixels, data.TotalPixels),
				Inline: true,
			},
		},
		Timestamp: time.Now().Format(time.RFC3339),
		Footer: &discordgo.MessageEmbedFooter{
			Text: "自動通知システム",
		},
	}

	if data.WeightedDiffPercentage != nil {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "🔍 加重差分率 (菊重視)",
			Value:  fmt.Sprintf("%.2f%%", *data.WeightedDiffPercentage),
			Inline: true,
		})
	}

	if data.ChrysanthemumDiffPixels > 0 || data.BackgroundDiffPixels > 0 {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "🔍 差分ピクセル (菊/背景)",
			Value:  fmt.Sprintf("菊 %d / %d | 背景 %d / %d", data.ChrysanthemumDiffPixels, data.ChrysanthemumTotalPixels, data.BackgroundDiffPixels, data.BackgroundTotalPixels),
			Inline: false,
		})
	}

	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
		Name:   "📐 監視ピクセル数",
		Value:  fmt.Sprintf("全体 %d | 菊 %d | 背景 %d", data.TotalPixels, data.ChrysanthemumTotalPixels, data.BackgroundTotalPixels),
		Inline: false,
	})
	appendCurrentDiffUserSummaryField(n, embed)
	appendMainMonitorMapField(embed)

	var files []*discordgo.File
	images := n.monitor.GetLatestImages()
	if images != nil && images.LiveImage != nil && images.DiffImage != nil {
		combinedImage, err := embeds.CombineImages(images.LiveImage, images.DiffImage)
		if err == nil {
			files = append(files, &discordgo.File{
				Name:        "koukyo_status.png",
				ContentType: "image/png",
				Reader:      combinedImage,
			})
			embed.Image = &discordgo.MessageEmbedImage{
				URL: "attachment://koukyo_status.png",
			}
		} else {
			log.Printf("Failed to combine images for decrease notification: %v", err)
		}
	}

	_, err := n.session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: message,
		Embeds:  []*discordgo.MessageEmbed{embed},
		Files:   files,
	})

	if err != nil {
		log.Printf("Failed to send decrease notification to channel %s: %v", channelID, err)
	} else {
		log.Printf("Decrease notification sent to guild %s: %.2f%%", guildID, diffValue)
	}
}

// sendZeroRecoveryNotification 0%からの回復通知を送信
func (n *Notifier) sendZeroRecoveryNotification(
	guildID string,
	settings config.GuildSettings,
	data *monitor.MonitorData,
	diffValue float64,
) {
	channelID := *settings.NotificationChannel

	metricLabel := "差分率"
	if settings.NotificationMetric == "weighted" {
		metricLabel = "加重差分率"
	}

	message := fmt.Sprintf("🔔 【Wplace速報】変化検知 %s: **%.2f%%**に上昇", metricLabel, diffValue)

	embed := &discordgo.MessageEmbed{
		Title:       "🟢 Wplace 変化検知",
		Description: fmt.Sprintf("完全な0%%から変動しました\n現在の%s: **%.2f%%**", metricLabel, diffValue),
		Color:       0x00FF00, // 緑
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "📊 差分率 (全体)",
				Value:  fmt.Sprintf("%.2f%%", data.DiffPercentage),
				Inline: true,
			},
			{
				Name:   "📈 差分ピクセル (全体)",
				Value:  fmt.Sprintf("%d / %d", data.DiffPixels, data.TotalPixels),
				Inline: true,
			},
		},
		Timestamp: time.Now().Format(time.RFC3339),
		Footer: &discordgo.MessageEmbedFooter{
			Text: "自動通知システム - 省電力モード解除",
		},
	}

	if data.WeightedDiffPercentage != nil {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "🔍 加重差分率 (菊重視)",
			Value:  fmt.Sprintf("%.2f%%", *data.WeightedDiffPercentage),
			Inline: true,
		})
	}

	if data.ChrysanthemumDiffPixels > 0 || data.BackgroundDiffPixels > 0 {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "🔍 差分ピクセル (菊/背景)",
			Value:  fmt.Sprintf("菊 %d / %d | 背景 %d / %d", data.ChrysanthemumDiffPixels, data.ChrysanthemumTotalPixels, data.BackgroundDiffPixels, data.BackgroundTotalPixels),
			Inline: false,
		})
	}

	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
		Name:   "📐 監視ピクセル数",
		Value:  fmt.Sprintf("全体 %d | 菊 %d | 背景 %d", data.TotalPixels, data.ChrysanthemumTotalPixels, data.BackgroundTotalPixels),
		Inline: false,
	})
	appendCurrentDiffUserSummaryField(n, embed)
	appendMainMonitorMapField(embed)

	var files []*discordgo.File
	images := n.monitor.GetLatestImages()
	if images != nil && images.LiveImage != nil && images.DiffImage != nil {
		combinedImage, err := embeds.CombineImages(images.LiveImage, images.DiffImage)
		if err == nil {
			files = append(files, &discordgo.File{
				Name:        "koukyo_status.png",
				ContentType: "image/png",
				Reader:      combinedImage,
			})
			embed.Image = &discordgo.MessageEmbedImage{
				URL: "attachment://koukyo_status.png",
			}
		} else {
			log.Printf("Failed to combine images for zero recovery notification: %v", err)
		}
	}

	_, err := n.session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: message,
		Embeds:  []*discordgo.MessageEmbed{embed},
		Files:   files,
	})

	if err != nil {
		log.Printf("Failed to send zero recovery notification to channel %s: %v", channelID, err)
	} else {
		log.Printf("Zero recovery notification sent to guild %s: %.2f%%", guildID, diffValue)
	}
}

// sendZeroCompletionNotification 0%に戻った時の通知を送信
func (n *Notifier) sendZeroCompletionNotification(
	guildID string,
	settings config.GuildSettings,
	data *monitor.MonitorData,
) {
	channelID := *settings.NotificationChannel

	metricLabel := "差分率"
	if settings.NotificationMetric == "weighted" {
		metricLabel = "加重差分率"
	}

	message := fmt.Sprintf("✅ 【Wplace速報】修復完了！ %s: **0.00%%** # Pixel Perfect!", metricLabel)

	embed := &discordgo.MessageEmbed{
		Title:       "🎉 Wplace 修復完了",
		Description: fmt.Sprintf("%sが0%%に戻りました\n# Pixel Perfect!", metricLabel),
		Color:       0x00FF00, // 緑
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "📊 差分率 (全体)",
				Value:  "0.00%",
				Inline: true,
			},
			{
				Name:   "📈 差分ピクセル (全体)",
				Value:  fmt.Sprintf("0 / %d", data.TotalPixels),
				Inline: true,
			},
		},
		Timestamp: time.Now().Format(time.RFC3339),
		Footer: &discordgo.MessageEmbedFooter{
			Text: "自動通知システム - 修復完了",
		},
	}

	if data.WeightedDiffPercentage != nil {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "🔍 加重差分率 (菊重視)",
			Value:  "0.00%",
			Inline: true,
		})
	}

	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
		Name:   "📐 監視ピクセル数",
		Value:  fmt.Sprintf("全体 %d | 菊 %d | 背景 %d", data.TotalPixels, data.ChrysanthemumTotalPixels, data.BackgroundTotalPixels),
		Inline: false,
	})
	appendCurrentDiffUserSummaryField(n, embed)
	appendMainMonitorMapField(embed)

	var files []*discordgo.File
	images := n.monitor.GetLatestImages()
	if images != nil && images.LiveImage != nil && images.DiffImage != nil {
		combinedImage, err := embeds.CombineImages(images.LiveImage, images.DiffImage)
		if err == nil {
			files = append(files, &discordgo.File{
				Name:        "koukyo_status.png",
				ContentType: "image/png",
				Reader:      combinedImage,
			})
			embed.Image = &discordgo.MessageEmbedImage{
				URL: "attachment://koukyo_status.png",
			}
		} else {
			log.Printf("Failed to combine images for zero completion notification: %v", err)
		}
	}

	_, err := n.session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: message,
		Embeds:  []*discordgo.MessageEmbed{embed},
		Files:   files,
	})

	if err != nil {
		log.Printf("Failed to send zero completion notification to channel %s: %v", channelID, err)
	} else {
		log.Printf("Zero completion notification sent to guild %s", guildID)
	}
}

func appendCurrentDiffUserSummaryField(n *Notifier, embed *discordgo.MessageEmbed) {
	if n == nil || n.monitor == nil || embed == nil {
		return
	}
	all := n.monitor.GetCurrentDiffPainterCounts(0)
	if len(all) == 0 {
		return
	}

	limit := diffUserSummaryTopN
	if limit <= 0 {
		limit = len(all)
	}
	if limit > len(all) {
		limit = len(all)
	}

	lines := make([]string, 0, limit+1)
	for i := 0; i < limit; i++ {
		item := all[i]
		lines = append(lines, fmt.Sprintf("%s | %dpx", utils.FormatUserDisplayName(item.Name, item.UserID), item.Pixels))
	}
	if len(all) > limit {
		lines = append(lines, fmt.Sprintf("...ほか%d人", len(all)-limit))
	}

	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
		Name:   "👥 同時検出ユーザー",
		Value:  strings.Join(lines, "\n"),
		Inline: false,
	})
}

// ResetState サーバーの通知状態をリセット
func (n *Notifier) ResetState(guildID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.states, guildID)
}

func (n *Notifier) NotifyNewUser(kind string, user activity.UserActivity) {
	switch kind {
	case "vandal":
		if n.vandalUserNotifier != nil {
			n.vandalUserNotifier.Notify(user)
		}
	case "fix":
		if n.fixUserNotifier != nil {
			n.fixUserNotifier.Notify(user)
		}
	}
}

// NotifyAchievement sends an achievement notification to the configured channel.
func (n *Notifier) NotifyAchievement(guildID, userDisplay, achievementName string) {
	if n == nil || n.session == nil || n.settings == nil {
		return
	}
	settings := n.settings.GetGuildSettings(guildID)
	if settings.AchievementChannel == nil {
		return
	}
	channelID := *settings.AchievementChannel
	content := fmt.Sprintf("🏅 %s が実績: **%s** を獲得しました！", userDisplay, achievementName)
	if _, err := n.session.ChannelMessageSend(channelID, content); err != nil {
		log.Printf("Failed to send achievement notification to channel %s: %v", channelID, err)
	}
}
