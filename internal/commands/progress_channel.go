package commands

import (
	"Koukyo_discord_bot/internal/config"
	"strings"

	"github.com/bwmarrin/discordgo"
)

const (
	progressActOn  = "on"
	progressActOff = "off"
)

type ProgressChannelCommand struct {
	settings *config.SettingsManager
}

func NewProgressChannelCommand(settings *config.SettingsManager) *ProgressChannelCommand {
	return &ProgressChannelCommand{settings: settings}
}

func (c *ProgressChannelCommand) Name() string {
	return "progresschannel"
}

func (c *ProgressChannelCommand) Description() string {
	return "ピクセルアート進捗通知チャンネルを設定します"
}

func (c *ProgressChannelCommand) ExecuteText(s *discordgo.Session, m *discordgo.MessageCreate, args []string) error {
	if !isAdmin(s, m.GuildID, m.Author.ID) {
		_, err := s.ChannelMessageSend(m.ChannelID, "❌ このコマンドは管理者のみ使用できます。")
		return err
	}
	if len(args) == 0 {
		_, err := s.ChannelMessageSend(m.ChannelID, "❌ 使用方法: `!progresschannel on` または `!progresschannel off`")
		return err
	}
	act := normalizeProgressAct(args[0])
	if act == "" {
		_, err := s.ChannelMessageSend(m.ChannelID, "❌ act は on または off を指定してください。")
		return err
	}
	return c.setProgressChannel(m.GuildID, m.ChannelID, act, func(msg string) error {
		_, err := s.ChannelMessageSend(m.ChannelID, msg)
		return err
	})
}

func (c *ProgressChannelCommand) ExecuteSlash(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	if !isAdmin(s, i.GuildID, interactionUserID(i)) {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ このコマンドは管理者のみ使用できます。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	act := ""
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "act" {
			act = normalizeProgressAct(opt.StringValue())
		}
	}
	if act == "" {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ act は on または off を指定してください。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	return c.setProgressChannel(i.GuildID, i.ChannelID, act, func(msg string) error {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: msg,
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	})
}

func (c *ProgressChannelCommand) SlashDefinition() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Name:        c.Name(),
		Description: c.Description(),
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "act",
				Description: "on/off",
				Required:    true,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "ON", Value: progressActOn},
					{Name: "OFF", Value: progressActOff},
				},
			},
		},
	}
}

func normalizeProgressAct(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case progressActOn:
		return progressActOn
	case progressActOff:
		return progressActOff
	default:
		return ""
	}
}

func (c *ProgressChannelCommand) setProgressChannel(guildID, channelID, act string, responder func(string) error) error {
	c.settings.UpdateGuildSetting(guildID, func(gs *config.GuildSettings) {
		if act == progressActOn {
			gs.ProgressChannel = &channelID
			gs.ProgressNotifyEnabled = true
		} else {
			gs.ProgressNotifyEnabled = false
		}
	})
	if act == progressActOn {
		return responder("✅ 進捗通知チャンネルをこのチャンネルに設定しました。")
	}
	return responder("✅ 進捗通知を無効にしました。")
}
