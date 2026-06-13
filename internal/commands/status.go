package commands

import (
	"Koukyo_discord_bot/internal/embeds"
	"Koukyo_discord_bot/internal/models"
	"Koukyo_discord_bot/internal/notifications"
	"fmt"

	"github.com/bwmarrin/discordgo"
)

type StatusCommand struct {
	botInfo  *models.BotInfo
	notifier *notifications.Notifier
}

func NewStatusCommand(botInfo *models.BotInfo, notifier *notifications.Notifier) *StatusCommand {
	return &StatusCommand{
		botInfo:  botInfo,
		notifier: notifier,
	}
}

func (c *StatusCommand) Name() string {
	return "status"
}

func (c *StatusCommand) Description() string {
	return "🤖 Bot自体のステータス（稼働時間、メモリ、通知統計）を表示します"
}

func (c *StatusCommand) ExecuteText(s *discordgo.Session, m *discordgo.MessageCreate, args []string) error {
	embed := embeds.BuildStatusEmbed(c.botInfo, s)

	// 通知ドロップ統計を追加
	if c.notifier != nil {
		high, low := c.notifier.GetDroppedNotificationStats()
		if high > 0 || low > 0 {
			embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
				Name:   "⚠️ ドロップされた通知",
				Value:  fmt.Sprintf("高優先度: %d件\n低優先度: %d件", high, low),
				Inline: false,
			})
		}
	}

	_, err := s.ChannelMessageSendEmbed(m.ChannelID, embed)
	return err
}

func (c *StatusCommand) ExecuteSlash(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	embed := embeds.BuildStatusEmbed(c.botInfo, s)

	// 通知ドロップ統計を追加
	if c.notifier != nil {
		high, low := c.notifier.GetDroppedNotificationStats()
		if high > 0 || low > 0 {
			embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
				Name:   "⚠️ ドロップされた通知",
				Value:  fmt.Sprintf("高優先度: %d件\n低優先度: %d件", high, low),
				Inline: false,
			})
		}
	}

	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
		},
	})
}

func (c *StatusCommand) SlashDefinition() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Name:        c.Name(),
		Description: c.Description(),
	}
}
