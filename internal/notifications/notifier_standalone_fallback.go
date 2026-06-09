package notifications

import (
	"Koukyo_discord_bot/internal/monitor"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

const (
	standaloneTriggerAfter     = 1 * time.Minute
	standaloneBaseInterval     = 2 * time.Second
	standaloneMaxInterval      = 5 * time.Minute
	standaloneErrorNotifyEvery = 10 * time.Minute
	kikuTemplateFile           = "1818-806-989-358_kiku_only.webp"
)

var forceStandaloneMode = os.Getenv("MONITOR_FORCE_STANDALONE") == "1"

func (n *Notifier) maybeRunStandaloneFallback(now time.Time) {
	if n == nil || n.monitor == nil || n.watchTargetsState == nil {
		return
	}

	wsUnavailable := forceStandaloneMode || n.monitor.IsWSUnavailableFor(standaloneTriggerAfter)
	if !wsUnavailable {
		n.leaveStandaloneFallbackIfActive(now)
		n.resetStandaloneScheduleLocked()
		return
	}
	n.enterStandaloneFallbackIfNeeded(now)

	n.standaloneMu.Lock()
	if !n.standaloneNextRun.IsZero() && now.Before(n.standaloneNextRun) {
		n.standaloneMu.Unlock()
		return
	}
	n.standaloneMu.Unlock()

	cfg, err := n.resolveStandaloneTarget()
	if err != nil {
		n.scheduleStandaloneFailure(now, err)
		log.Printf("standalone fallback: target resolve failed: %v", err)
		return
	}

	result, err := n.buildWatchTargetResult(cfg)
	if err != nil {
		n.scheduleStandaloneFailure(now, err)
		log.Printf("standalone fallback: build failed target=%s err=%v", cfg.ID, err)
		return
	}

	data := &monitor.MonitorData{
		Type:           "standalone",
		DiffPercentage: result.percent,
		DiffPixels:     result.diffPixels,
		TotalPixels:    result.template.OpaqueCount,
	}

	// 菊のみテンプレートで加重差分を計算する。
	kikuTemplate, kikuErr := n.watchTargetsState.loadTemplateFromDataDir(n.dataDir, kikuTemplateFile)
	if kikuErr == nil {
		if liveImg, decodeErr := decodePNGToNRGBA(result.livePNG); decodeErr == nil {
			kikuDiff, _ := buildDiffMask(kikuTemplate.Img, liveImg)
			weighted := float64(kikuDiff) * 100 / float64(kikuTemplate.OpaqueCount)
			data.WeightedDiffPercentage = &weighted
			data.ChrysanthemumDiffPixels = kikuDiff
			data.ChrysanthemumTotalPixels = kikuTemplate.OpaqueCount
			bgDiff := result.diffPixels - kikuDiff
			if bgDiff < 0 {
				bgDiff = 0
			}
			data.BackgroundDiffPixels = bgDiff
			data.BackgroundTotalPixels = result.template.OpaqueCount - kikuTemplate.OpaqueCount
		} else {
			log.Printf("standalone fallback: failed to decode live image for kiku diff: %v", decodeErr)
		}
	} else {
		log.Printf("standalone fallback: kiku template unavailable, skipping weighted diff: %v", kikuErr)
	}

	n.monitor.State.UpdateData(data)
	n.monitor.State.UpdateImages(&monitor.ImageData{
		LiveImage: result.livePNG,
		DiffImage: result.diffPNG,
		Timestamp: now,
	})
	n.monitor.EnqueueDiffImageToTracker(result.diffPNG)
	n.scheduleStandaloneSuccess(now)
}

func (n *Notifier) resolveStandaloneTarget() (watchTargetConfig, error) {
	targetID := strings.TrimSpace(os.Getenv("MONITOR_STANDALONE_TARGET_ID"))
	if targetID != "" && n.watchTargetsState != nil {
		targets, err := n.watchTargetsState.loadConfigs()
		if err == nil && len(targets) > 0 {
			for _, t := range targets {
				if targetIDMatches(t, targetID) {
					return t, nil
				}
			}
			return watchTargetConfig{}, fmt.Errorf("MONITOR_STANDALONE_TARGET_ID not found: %s", targetID)
		}
		return watchTargetConfig{}, fmt.Errorf("failed to load watch targets for MONITOR_STANDALONE_TARGET_ID")
	}

	origin := strings.TrimSpace(os.Getenv("MONITOR_STANDALONE_ORIGIN"))
	template := strings.TrimSpace(os.Getenv("MONITOR_STANDALONE_TEMPLATE"))
	if origin == "" {
		origin = "1818-806-989-358"
	}
	if template == "" {
		template = "1818-806-989-358.png"
	}
	return watchTargetConfig{
		ID:       "standalone-default",
		Label:    "Standalone Default",
		Origin:   origin,
		Template: template,
		Interval: standaloneBaseInterval,
	}, nil
}

func (n *Notifier) resetStandaloneScheduleLocked() {
	n.standaloneMu.Lock()
	n.standaloneAttempts = 0
	n.standaloneNextRun = time.Time{}
	n.standaloneErrorCount = 0
	n.standaloneLastError = ""
	n.standaloneLastErrorAt = time.Time{}
	n.standaloneLastErrorNotif = time.Time{}
	n.standaloneMu.Unlock()
}

func (n *Notifier) scheduleStandaloneFailure(now time.Time, err error) {
	n.maybeNotifyStandaloneErrorSummary(now, err)

	n.standaloneMu.Lock()
	attempt := n.standaloneAttempts
	if attempt > 5 {
		attempt = 5
	}
	delay := standaloneBaseInterval * time.Duration(1<<uint(attempt))
	if delay > standaloneMaxInterval {
		delay = standaloneMaxInterval
	}
	n.standaloneNextRun = now.Add(delay)
	if n.standaloneAttempts < 5 {
		n.standaloneAttempts++
	}
	n.standaloneMu.Unlock()
}

func (n *Notifier) scheduleStandaloneSuccess(now time.Time) {
	n.standaloneMu.Lock()
	n.standaloneAttempts = 0
	n.standaloneNextRun = now.Add(standaloneBaseInterval)
	n.standaloneErrorCount = 0
	n.standaloneLastError = ""
	n.standaloneLastErrorAt = time.Time{}
	n.standaloneLastErrorNotif = time.Time{}
	n.standaloneMu.Unlock()
}

func (n *Notifier) enterStandaloneFallbackIfNeeded(now time.Time) {
	n.standaloneMu.Lock()
	if n.standaloneActive {
		n.standaloneMu.Unlock()
		return
	}
	n.standaloneActive = true
	n.standaloneStartedAt = now
	n.standaloneMu.Unlock()

	if forceStandaloneMode {
		n.notifyStandaloneToGuilds("🔧 MONITOR_FORCE_STANDALONE が有効のため、起動時からスタンドアロンモードで動作しています。")
	} else {
		n.notifyStandaloneToGuilds("⚠️ WS断が1分以上継続したため、スタンドアロン取得にフォールバックしました。")
	}
}

func (n *Notifier) leaveStandaloneFallbackIfActive(now time.Time) {
	n.standaloneMu.Lock()
	if !n.standaloneActive {
		n.standaloneMu.Unlock()
		return
	}
	startedAt := n.standaloneStartedAt
	n.standaloneActive = false
	n.standaloneStartedAt = time.Time{}
	n.standaloneMu.Unlock()

	duration := now.Sub(startedAt).Round(time.Second)
	n.notifyStandaloneToGuilds(fmt.Sprintf("✅ WS接続が復帰したため、スタンドアロンフォールバックを終了しました（継続時間: %s）。", duration))
}

func (n *Notifier) maybeNotifyStandaloneErrorSummary(now time.Time, err error) {
	n.standaloneMu.Lock()
	n.standaloneErrorCount++
	n.standaloneLastError = err.Error()
	n.standaloneLastErrorAt = now

	shouldNotify := n.standaloneLastErrorNotif.IsZero() || now.Sub(n.standaloneLastErrorNotif) >= standaloneErrorNotifyEvery
	if !shouldNotify {
		n.standaloneMu.Unlock()
		return
	}

	count := n.standaloneErrorCount
	lastErr := n.standaloneLastError
	lastErrAt := n.standaloneLastErrorAt
	n.standaloneErrorCount = 0
	n.standaloneLastErrorNotif = now
	n.standaloneMu.Unlock()

	msg := fmt.Sprintf(
		"⚠️ スタンドアロンフォールバックでエラーが発生しています（直近%d件）。最新エラー: `%s`（%s）",
		count,
		lastErr,
		lastErrAt.Format("15:04:05"),
	)
	n.notifyStandaloneToGuilds(msg)
}

func (n *Notifier) notifyStandaloneToGuilds(content string) {
	if n == nil || n.session == nil || n.settings == nil || strings.TrimSpace(content) == "" {
		return
	}
	for _, guild := range n.session.State.Guilds {
		guildID := guild.ID
		gs := n.settings.GetGuildSettings(guildID)
		if !gs.AutoNotifyEnabled || gs.NotificationChannel == nil {
			continue
		}
		channelID := *gs.NotificationChannel
		n.EnqueueHigh(func() {
			if _, err := n.session.ChannelMessageSend(channelID, content); err != nil {
				log.Printf("standalone fallback notification failed guild=%s channel=%s err=%v", guildID, channelID, err)
			}
		})
	}
}
