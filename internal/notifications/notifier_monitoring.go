package notifications

import (
	"Koukyo_discord_bot/internal/embeds"
	"Koukyo_discord_bot/internal/monitor"
	"bytes"
	"fmt"
	"log"
	"time"

	"github.com/bwmarrin/discordgo"
)

// StartMonitoring 全サーバーの監視を開始
func (n *Notifier) StartMonitoring() {
	n.startDailyRankingLoop()
	n.startWatchTargetsLoop()
	n.startProgressTargetsLoop()
	n.startAchievementLoop()
	n.startDispatchWorker()
	n.startWplaceHealthLoop()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PANIC in notifier StartMonitoring loop: %v", r)
			}
		}()

		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		// lastHeartbeat := time.Now()
		for range ticker.C {
			n.maybeRunStandaloneFallback(time.Now())

			// Lightweight heartbeat to detect a stuck monitoring loop.
			// if time.Since(lastHeartbeat) >= 60*time.Second {
			// 	lastHeartbeat = time.Now()
			// 	log.Printf("notifier: monitoring heartbeat power_save=%v guilds=%d", n.monitor.State.IsPowerSaveMode(), len(n.session.State.Guilds))
			// }

			// 監視データが更新されたら全サーバーをチェック
			if !n.monitor.State.HasData() {
				continue
			}

			currentPowerSave := n.monitor.State.IsPowerSaveMode()
			if n.lastPowerSaveMode && !currentPowerSave {
				// Reset small-diff editable message pointers so post-resume updates
				// don't edit stale messages that sit above the resume notification.
				n.resetAllSmallDiffMessageTracking()
				// For debugging: notify resume, but never block the monitoring loop.
				go n.notifyPowerSaveResume()
			}
			if !n.lastPowerSaveMode && currentPowerSave {
				// Entering power-save also resets pointers to avoid cross-cycle edits.
				n.resetAllSmallDiffMessageTracking()
			}
			n.lastPowerSaveMode = currentPowerSave

			if currentPowerSave {
				continue
			}

			// Botが参加している全サーバーをチェック
			for _, guild := range n.session.State.Guilds {
				guildID := guild.ID
				n.CheckAndNotify(guildID)
			}

			// DM速報チェック
			n.CheckAndNotifyDM()

			// タイムラプス完了の自動投稿
			t := n.monitor.State.GetTimelapseCompletedAt()
			if t != nil && (n.lastTimelapseCompletedAt == nil || t.After(*n.lastTimelapseCompletedAt)) {
				tt := *t
				n.lastTimelapseCompletedAt = &tt // set early to avoid re-trigger loops

				frames := n.monitor.State.GetLastTimelapseFrames()
				if len(frames) > 0 {
					// Timelapse can be heavy and spammy; never block monitoring.
					go func(fr []monitor.TimelapseFrame) {
						n.timelapsePostMu.Lock()
						if n.timelapsePosting {
							n.timelapsePostMu.Unlock()
							return
						}
						n.timelapsePosting = true
						n.timelapsePostMu.Unlock()

						defer func() {
							n.timelapsePostMu.Lock()
							n.timelapsePosting = false
							n.timelapsePostMu.Unlock()
						}()

						n.postTimelapseToGuilds(fr)
					}(frames)
				}
			}

		}
	}()

	log.Println("Notification monitoring started")
}

func (n *Notifier) notifyPowerSaveResume() {
	for _, guild := range n.session.State.Guilds {
		gs := n.settings.GetGuildSettings(guild.ID)
		if !gs.AutoNotifyEnabled || gs.NotificationChannel == nil {
			continue
		}
		_, err := n.session.ChannelMessageSend(
			*gs.NotificationChannel,
			"🌅 省電力モードを解除しました。更新を再開します。",
		)
		if err != nil {
			log.Printf("Failed to send power-save resume notification to guild %s: %v", guild.ID, err)
		}
	}
}

func (n *Notifier) postTimelapseToGuilds(frames []monitor.TimelapseFrame) {
	gifBuf, err := embeds.BuildTimelapseGIF(frames)
	if err != nil {
		log.Printf("Failed to build timelapse GIF: %v", err)
		return
	}
	if gifBuf.Len() == 0 {
		log.Printf("Failed to build timelapse GIF: empty buffer")
		return
	}
	frameCount := len(frames)
	startTime := frames[0].Timestamp
	endTime := frames[frameCount-1].Timestamp
	duration := endTime.Sub(startTime)
	jst := time.FixedZone("JST", 9*3600)

	// 投稿対象ギルド
	for _, guild := range n.session.State.Guilds {
		gs := n.settings.GetGuildSettings(guild.ID)
		if !gs.AutoNotifyEnabled || gs.NotificationChannel == nil {
			continue
		}
		reader := bytes.NewReader(gifBuf.Bytes())
		embed := &discordgo.MessageEmbed{
			Title:       "📽️ タイムラプス完了",
			Description: "差分率 30%→0.2% の期間を自動生成しました",
			Color:       0x00AA88,
			Fields: []*discordgo.MessageEmbedField{
				{
					Name:   "期間",
					Value:  fmt.Sprintf("%s ～ %s (JST)", startTime.In(jst).Format("2006-01-02 15:04:05"), endTime.In(jst).Format("2006-01-02 15:04:05")),
					Inline: false,
				},
				{
					Name:   "フレーム数",
					Value:  fmt.Sprintf("%d", frameCount),
					Inline: true,
				},
				{
					Name:   "生成時間",
					Value:  fmt.Sprintf("%.1f秒", duration.Seconds()),
					Inline: true,
				},
			},
			Timestamp: time.Now().Format(time.RFC3339),
		}
		_, err := n.session.ChannelMessageSendComplex(*gs.NotificationChannel, &discordgo.MessageSend{
			Embeds: []*discordgo.MessageEmbed{embed},
			Files: []*discordgo.File{{
				Name:        "timelapse.gif",
				ContentType: "image/gif",
				Reader:      reader,
			}},
		})
		if err != nil {
			log.Printf("Failed to post timelapse to guild %s: %v", guild.ID, err)
		} else {
			log.Printf("Posted timelapse to guild %s", guild.ID)
		}
	}
}
