package handler

import (
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
}

func (h *Handler) OnResumed(s *discordgo.Session, event *discordgo.Resumed) {
	log.Println("Discord gateway session resumed")
}
