package commands

import (
	"strings"

	"github.com/bwmarrin/discordgo"
)

type ProxyDeleteCommand struct{}

func NewProxyDeleteCommand() *ProxyDeleteCommand {
	return &ProxyDeleteCommand{}
}

func (c *ProxyDeleteCommand) Name() string {
	return "proxydelete"
}

func (c *ProxyDeleteCommand) Description() string {
	return "代理投稿メッセージを削除します（管理者専用）"
}

func (c *ProxyDeleteCommand) ExecuteText(s *discordgo.Session, m *discordgo.MessageCreate, args []string) error {
	if m.GuildID == "" {
		_, err := s.ChannelMessageSend(m.ChannelID, "❌ このコマンドはサーバー内でのみ利用できます。")
		return err
	}
	if !isAdmin(s, m.GuildID, m.Author.ID) {
		_, err := s.ChannelMessageSend(m.ChannelID, "❌ このコマンドは管理者のみ使用できます。")
		return err
	}
	if len(args) < 1 {
		_, err := s.ChannelMessageSend(m.ChannelID, "❌ 使用方法: `!proxydelete message_id`")
		return err
	}
	messageID := strings.TrimSpace(args[0])
	if messageID == "" {
		_, err := s.ChannelMessageSend(m.ChannelID, "❌ message_id を指定してください。")
		return err
	}

	if err := s.ChannelMessageDelete(m.ChannelID, messageID); err != nil {
		_, sendErr := s.ChannelMessageSend(m.ChannelID, "❌ 削除に失敗しました: "+err.Error())
		return sendErr
	}

	_, err := s.ChannelMessageSend(m.ChannelID, "✅ メッセージを削除しました。")
	return err
}

func (c *ProxyDeleteCommand) ExecuteSlash(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	if i.GuildID == "" {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ このコマンドはサーバー内でのみ利用できます。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}
	requesterID := ""
	if i.Member != nil && i.Member.User != nil {
		requesterID = i.Member.User.ID
	} else if i.User != nil {
		requesterID = i.User.ID
	}
	if !isAdmin(s, i.GuildID, requesterID) {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ このコマンドは管理者のみ使用できます。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	messageID := ""
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "message_id" {
			messageID = strings.TrimSpace(opt.StringValue())
		}
	}
	if messageID == "" {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ message_id を指定してください。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	if err := s.ChannelMessageDelete(i.ChannelID, messageID); err != nil {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ 削除に失敗しました: " + err.Error(),
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "✅ メッセージを削除しました。",
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

func (c *ProxyDeleteCommand) SlashDefinition() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Name:        c.Name(),
		Description: c.Description(),
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "message_id",
				Description: "削除したいメッセージID（同じチャンネル内）",
				Required:    true,
			},
		},
	}
}
