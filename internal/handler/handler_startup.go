package handler

import (
	"Koukyo_discord_bot/internal/embeds"
	"log"
	"time"

	"github.com/bwmarrin/discordgo"
)

func (h *Handler) OnReady(s *discordgo.Session, event *discordgo.Ready) {
	h.readyMu.Lock()
	firstReady := !h.readyInitialized
	h.readyInitialized = true
	h.lastReadyAt = time.Now()
	h.readyMu.Unlock()

	log.Println("Bot is ready!")
	log.Printf("Logged in as: %s#%s", s.State.User.Username, s.State.User.Discriminator)

	if !firstReady {
		log.Println("Gateway reconnected (READY). Skipping startup notifications and slash sync.")
		return
	}

	// 初回READY時のみスラッシュコマンドを同期
	if err := h.SyncSlashCommands(s); err != nil {
		log.Printf("Error syncing slash commands: %v", err)
	}

	// 初回READY時のみ各ギルドに起動情報を送信
	// h.SendStartupNotification(s)
}

func (h *Handler) OnResumed(s *discordgo.Session, event *discordgo.Resumed) {
	log.Println("Discord gateway session resumed")
}

// SendStartupNotification 起動通知を各ギルドに送信
func (h *Handler) SendStartupNotification(s *discordgo.Session) {
	for _, guild := range s.State.Guilds {
		guildID := guild.ID
		settings := h.settings.GetGuildSettings(guildID)

		// 通知チャンネルが設定されていない場合は送信しない
		if settings.NotificationChannel == nil {
			continue
		}
		channelID := *settings.NotificationChannel

		// Bot起動通知を送信
		startupEmbed := embeds.BuildBotStartupEmbed(h.botInfo)
		_, err := s.ChannelMessageSendEmbed(channelID, startupEmbed)
		if err != nil {
			log.Printf("Error sending startup embed to guild %s: %v", guildID, err)
		}

		// 現在の監視情報を送信（データがある場合）
		if h.monitor != nil && h.monitor.State.HasData() {
			nowEmbed := embeds.BuildNowEmbed(h.monitor)
			images := h.monitor.GetLatestImages()
			if images != nil && len(images.LiveImage) > 0 && len(images.DiffImage) > 0 {
				combinedImage, err2 := embeds.CombineImages(images.LiveImage, images.DiffImage)
				if err2 == nil {
					nowEmbed.Image = &discordgo.MessageEmbedImage{URL: "attachment://koukyo_combined.png"}
					_, err = s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
						Embeds: []*discordgo.MessageEmbed{nowEmbed},
						Files: []*discordgo.File{{
							Name:   "koukyo_combined.png",
							Reader: combinedImage,
						}},
					})
				} else {
					log.Printf("Failed to combine images for startup now: %v", err2)
					_, err = s.ChannelMessageSendEmbed(channelID, nowEmbed)
				}
			} else {
				_, err = s.ChannelMessageSendEmbed(channelID, nowEmbed)
			}

			if err != nil {
				log.Printf("Error sending now embed to guild %s: %v", guildID, err)
			}
		}
	}
}
