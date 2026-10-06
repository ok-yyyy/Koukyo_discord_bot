package handler

import (
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"
)

func (h *Handler) OnMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Botメッセージを無視
	if m.Author.Bot {
		return
	}

	// プレフィックスチェック
	if !strings.HasPrefix(m.Content, h.prefix) {
		return
	}

	// コマンドと引数をパース
	content := strings.TrimPrefix(m.Content, h.prefix)
	parts := strings.Fields(content)
	if len(parts) == 0 {
		return
	}

	cmdName := strings.ToLower(parts[0])
	args := parts[1:]

	log.Printf("Text command received: %s (args=%d) from %s", cmdName, len(args), m.Author.Username)

	// コマンド実行
	cmd, exists := h.registry.Get(cmdName)
	if !exists {
		if h.handleUnknownTextCommand(s, m, cmdName) {
			return
		}
		log.Printf("Command '%s' not found in registry", cmdName)
		return
	}

	log.Printf("Executing text command: %s", cmdName)
	if err := cmd.ExecuteText(s, m, args); err != nil {
		log.Printf("Error executing command %s: %v", cmdName, err)
		s.ChannelMessageSend(m.ChannelID, "An error occurred while executing the command.")
	} else {
		log.Printf("Command %s completed successfully", cmdName)
	}
}

func (h *Handler) handleUnknownTextCommand(s *discordgo.Session, m *discordgo.MessageCreate, cmdName string) bool {
	fallbacks := []func(*discordgo.Session, *discordgo.MessageCreate, string) bool{
		func(_ *discordgo.Session, m *discordgo.MessageCreate, targetID string) bool {
			return h.notifier != nil && h.notifier.HandleProgressTargetManual(m.ChannelID, targetID)
		},
		func(_ *discordgo.Session, m *discordgo.MessageCreate, targetID string) bool {
			return h.notifier != nil && h.notifier.HandleWatchTargetManual(m.ChannelID, targetID)
		},
		h.handleGetShortcut,
		h.handleEasterEgg,
	}
	for _, handler := range fallbacks {
		if handler(s, m, cmdName) {
			return true
		}
	}
	return false
}
