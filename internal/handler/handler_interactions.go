package handler

import (
	"Koukyo_discord_bot/internal/commands"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// OnInteractionCreate スラッシュコマンド・ボタン・モーダルハンドラー
func (h *Handler) OnInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	log.Printf("Interaction received: Type=%d", i.Type)

	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		// スラッシュコマンド
		h.handleSlashCommand(s, i)
	case discordgo.InteractionMessageComponent:
		// ボタンやセレクトメニュー
		h.handleMessageComponent(s, i)
	case discordgo.InteractionModalSubmit:
		// モーダル送信
		h.handleModalSubmit(s, i)
	default:
		log.Printf("Unknown interaction type: %d", i.Type)
	}
}

func (h *Handler) handleSlashCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	cmdName := i.ApplicationCommandData().Name
	log.Printf("Slash command: /%s", cmdName)

	cmd, exists := h.registry.Get(cmdName)
	if !exists {
		log.Printf("Unknown slash command: %s", cmdName)
		return
	}

	log.Printf("Executing slash command: /%s", cmdName)
	if err := cmd.ExecuteSlash(s, i); err != nil {
		log.Printf("Error executing slash command %s: %v", cmdName, err)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "An error occurred while executing the command.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	} else {
		log.Printf("Slash command %s completed", cmdName)
	}
}

func (h *Handler) handleMessageComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	customID := i.MessageComponentData().CustomID
	log.Printf("Message component: %s", customID)

	componentHandlers := []struct {
		match  func(string) bool
		handle func()
	}{
		{
			match: func(id string) bool { return strings.HasPrefix(id, "settings_") },
			handle: func() {
				commands.HandleSettingsButtonInteraction(s, i, h.settings, h.notifier)
			},
		},
		{
			match: func(id string) bool { return strings.HasPrefix(id, "select_") },
			handle: func() {
				commands.HandleSettingsSelectMenu(s, i, h.settings)
			},
		},
		{
			match: func(id string) bool { return strings.HasPrefix(id, "userlist:") },
			handle: func() {
				commands.HandleUserListPagination(s, i, h.dataDir)
			},
		},
		{
			match: func(id string) bool { return strings.HasPrefix(id, "useractivity:") },
			handle: func() {
				commands.HandleUserActivityPagination(s, i, h.dataDir)
			},
		},
		{
			match: func(id string) bool { return id == "useractivity_select" },
			handle: func() {
				commands.HandleUserActivitySelect(s, i, h.dataDir)
			},
		},
		{
			match: func(id string) bool { return strings.HasPrefix(id, "regionmap_page:") },
			handle: func() {
				commands.HandleRegionMapPagination(s, i)
			},
		},
		{
			match: func(id string) bool { return strings.HasPrefix(id, "regionmap_select:") },
			handle: func() {
				commands.HandleRegionMapSelect(s, i)
			},
		},
		{
			match: func(id string) bool { return strings.HasPrefix(id, "regionmap_confirm:") },
			handle: func() {
				commands.HandleRegionMapConfirm(s, i, h.limiter)
			},
		},
	}

	for _, handler := range componentHandlers {
		if handler.match(customID) {
			handler.handle()
			return
		}
	}

	log.Printf("Unknown message component: %s", customID)
}

func (h *Handler) handleModalSubmit(s *discordgo.Session, i *discordgo.InteractionCreate) {
	modalID := i.ModalSubmitData().CustomID
	log.Printf("Modal submit: %s", modalID)

	// 設定モーダル
	if strings.HasPrefix(modalID, "modal_set_") {
		commands.HandleSettingsModalSubmit(s, i, h.settings, h.notifier)
		return
	}

	log.Printf("Unknown modal: %s", modalID)
}
