package commands

import (
	"Koukyo_discord_bot/internal/config"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

const (
	notificationTypeVandal = "vandal"
	notificationTypeFix    = "fix"
)

type NotificationCommand struct {
	settings *config.SettingsManager
}

func NewNotificationCommand(settings *config.SettingsManager) *NotificationCommand {
	return &NotificationCommand{settings: settings}
}

func (c *NotificationCommand) Name() string {
	return "notification"
}

func (c *NotificationCommand) Description() string {
	return "新規荒らし/修復ユーザーの通知チャンネルを設定します"
}

func (c *NotificationCommand) ExecuteText(s *discordgo.Session, m *discordgo.MessageCreate, args []string) error {
	if !isAdmin(s, m.GuildID, m.Author.ID) {
		_, err := s.ChannelMessageSend(m.ChannelID, "❌ このコマンドは管理者のみ使用できます。")
		return err
	}
	if len(args) == 0 {
		_, err := s.ChannelMessageSend(m.ChannelID, "❌ 使用方法: `!notification vandal` または `!notification fix`")
		return err
	}
	kind := normalizeNotificationType(args[0])
	if kind == "" {
		_, err := s.ChannelMessageSend(m.ChannelID, "❌ type は vandal または fix を指定してください。")
		return err
	}
	return c.setNotificationChannel(m.GuildID, m.ChannelID, kind, func(msg string) error {
		_, err := s.ChannelMessageSend(m.ChannelID, msg)
		return err
	})
}

func (c *NotificationCommand) ExecuteSlash(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	if !isAdmin(s, i.GuildID, interactionUserID(i)) {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ このコマンドは管理者のみ使用できます。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	kind := ""
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "type" {
			kind = normalizeNotificationType(opt.StringValue())
		}
	}
	if kind == "" {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ type は vandal または fix を指定してください。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	return c.setNotificationChannel(i.GuildID, i.ChannelID, kind, func(msg string) error {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: msg,
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	})
}

func (c *NotificationCommand) SlashDefinition() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Name:        c.Name(),
		Description: c.Description(),
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "type",
				Description: "通知タイプ (vandal / fix)",
				Required:    true,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "荒らし", Value: notificationTypeVandal},
					{Name: "修復", Value: notificationTypeFix},
				},
			},
		},
	}
}

func normalizeNotificationType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case notificationTypeVandal:
		return notificationTypeVandal
	case notificationTypeFix:
		return notificationTypeFix
	default:
		return ""
	}
}

func (c *NotificationCommand) setNotificationChannel(guildID, channelID, kind string, responder func(string) error) error {
	c.settings.UpdateGuildSetting(guildID, func(gs *config.GuildSettings) {
		if kind == notificationTypeVandal {
			gs.NotificationVandalChannel = &channelID
		} else {
			gs.NotificationFixChannel = &channelID
		}
	})
	return responder(fmt.Sprintf("✅ %s の通知チャンネルをこのチャンネルに設定しました。", kind))
}
