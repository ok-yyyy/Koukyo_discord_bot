package notifications

import (
	"fmt"
	"log"
	"sync"
	"time"

	"Koukyo_discord_bot/internal/monitor"
)

const (
	dmDiffThreshold  = 10.0           // 加重差分率の通知閾値（%）
	dmNotifyMetric   = "weighted"     // 常に加重差分率を使用
	dmNotifyCooldown = 3 * time.Minute // 連続通知を防ぐクールダウン
)

type dmUserState struct {
	mu         sync.Mutex
	lastTier   Tier
	wasZero    bool
	lastNotify time.Time
}

// CheckAndNotifyDM DM速報が有効な全ユーザーへの通知チェック
func (n *Notifier) CheckAndNotifyDM() {
	if n == nil || n.session == nil || n.settings == nil || n.monitor == nil {
		return
	}
	if n.monitor.State.IsPowerSaveMode() {
		return
	}

	data := n.monitor.GetLatestData()
	if data == nil {
		return
	}

	diffValue := getDiffValue(data, dmNotifyMetric)
	isZero := isZeroDiff(diffValue)
	currentTier := calculateTier(diffValue, dmDiffThreshold)

	for _, userID := range n.settings.GetDMEnabledUserIDs() {
		n.checkAndNotifyDMUser(userID, data, diffValue, isZero, currentTier)
	}
}

func (n *Notifier) getDMUserState(userID string) *dmUserState {
	n.dmUserStatesMu.Lock()
	defer n.dmUserStatesMu.Unlock()
	if s, ok := n.dmUserStates[userID]; ok {
		return s
	}
	s := &dmUserState{lastTier: TierNone, wasZero: true}
	n.dmUserStates[userID] = s
	return s
}

func (n *Notifier) checkAndNotifyDMUser(userID string, _ *monitor.MonitorData, diffValue float64, isZero bool, currentTier Tier) {
	state := n.getDMUserState(userID)
	state.mu.Lock()

	lastTier := state.lastTier
	wasZero := state.wasZero

	// 状態は毎ティック更新する（クールダウン中でも次回通知内容が正確になるよう）
	state.lastTier = currentTier
	state.wasZero = isZero

	var msg string
	switch {
	case wasZero && !isZero:
		msg = fmt.Sprintf("🔔 【Wplace速報 DM】変化検知 加重差分率: **%.2f%%**に上昇", diffValue)
	case !wasZero && isZero:
		msg = "✅ 【Wplace速報 DM】修復完了！ 加重差分率: **0.00%** # Pixel Perfect!"
	case !isZero && currentTier > lastTier:
		msg = fmt.Sprintf("🚨 【Wplace速報 DM】加重差分率が**%.2f%%**に増加しました！", diffValue)
	case !isZero && currentTier < lastTier:
		msg = fmt.Sprintf("📉 【Wplace速報 DM】加重差分率が**%.2f%%**に減少しました。", diffValue)
	}

	if msg == "" {
		state.mu.Unlock()
		return
	}

	// クールダウンチェック（差分振動によるDMスパムを防止）
	now := time.Now()
	if !state.lastNotify.IsZero() && now.Sub(state.lastNotify) < dmNotifyCooldown {
		state.mu.Unlock()
		return
	}
	state.lastNotify = now
	state.mu.Unlock()

	n.EnqueueHigh(func() {
		n.sendDMNotification(userID, msg)
	})
}

func (n *Notifier) sendDMNotification(userID, content string) {
	ch, err := n.session.UserChannelCreate(userID)
	if err != nil {
		log.Printf("DM notification: failed to open DM channel for user %s: %v", userID, err)
		return
	}
	if _, err := n.session.ChannelMessageSend(ch.ID, content); err != nil {
		log.Printf("DM notification: failed to send DM to user %s: %v", userID, err)
	}
}

// NotifyPaintRecovery Paintが全回復したことをユーザーにDMで通知します
func (n *Notifier) NotifyPaintRecovery(userID string, max int) {
	msg := fmt.Sprintf("🖌️ **Paint全回復通知**\nPaintが上限 (**%d**) まで回復しました！", max)
	n.sendDMNotification(userID, msg)
}
