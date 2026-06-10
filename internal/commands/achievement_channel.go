package commands

import (
	"Koukyo_discord_bot/internal/config"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

type AchievementChannelCommand struct {
	settings *config.SettingsManager
}

func NewAchievementChannelCommand(settings *config.SettingsManager) *AchievementChannelCommand {
	return &AchievementChannelCommand{settings: settings}
}

func (c *AchievementChannelCommand) Name() string { return "achievementchannel" }
func (c *AchievementChannelCommand) Description() string {
	return "実績通知の送信先チャンネルを設定します"
}

func (c *AchievementChannelCommand) ExecuteText(s *discordgo.Session, m *discordgo.MessageCreate, args []string) error {
	if !isAdmin(s, m.GuildID, m.Author.ID) {
		_, err := s.ChannelMessageSend(m.ChannelID, "❌ このコマンドは管理者のみ使用できます。")
		return err
	}
	if len(args) > 0 && strings.EqualFold(args[0], "off") {
		c.settings.UpdateGuildSetting(m.GuildID, func(gs *config.GuildSettings) {
			gs.AchievementChannel = nil
		})
		_, err := s.ChannelMessageSend(m.ChannelID, "✅ 実績通知チャンネルを解除しました。")
		return err
	}
	channelID := m.ChannelID
	c.settings.UpdateGuildSetting(m.GuildID, func(gs *config.GuildSettings) {
		gs.AchievementChannel = &channelID
	})
	_, err := s.ChannelMessageSend(m.ChannelID, "✅ このチャンネルを実績通知先に設定しました。")
	return err
}

func (c *AchievementChannelCommand) ExecuteSlash(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	if !isAdmin(s, i.GuildID, interactionUserID(i)) {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ このコマンドは管理者のみ使用できます。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	var targetChannelID string
	mode := "set"
	for _, opt := range i.ApplicationCommandData().Options {
		switch opt.Name {
		case "channel":
			targetChannelID = opt.ChannelValue(nil).ID
		case "mode":
			mode = opt.StringValue()
		}
	}

	if strings.EqualFold(mode, "off") || strings.EqualFold(mode, "disable") {
		c.settings.UpdateGuildSetting(i.GuildID, func(gs *config.GuildSettings) {
			gs.AchievementChannel = nil
		})
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "✅ 実績通知チャンネルを解除しました。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	if targetChannelID == "" {
		targetChannelID = i.ChannelID
	}

	c.settings.UpdateGuildSetting(i.GuildID, func(gs *config.GuildSettings) {
		gs.AchievementChannel = &targetChannelID
	})
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("✅ 実績通知チャンネルを <#%s> に設定しました。", targetChannelID),
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

func (c *AchievementChannelCommand) SlashDefinition() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Name:        c.Name(),
		Description: c.Description(),
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionChannel,
				Name:        "channel",
				Description: "実績通知の送信先チャンネル",
				Required:    false,
			},
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "mode",
				Description: "off を指定すると解除します",
				Required:    false,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "set", Value: "set"},
					{Name: "off", Value: "off"},
				},
			},
		},
	}
}
