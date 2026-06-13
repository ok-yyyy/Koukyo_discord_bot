package commands

import (
	"sort"

	"github.com/bwmarrin/discordgo"
)

type HelpCommand struct {
	registry *Registry
}

func NewHelpCommand(registry *Registry) *HelpCommand {
	return &HelpCommand{registry: registry}
}

func (c *HelpCommand) Name() string {
	return "help"
}

func (c *HelpCommand) Description() string {
	return "利用可能なコマンド一覧を表示します"
}

func (c *HelpCommand) ExecuteText(s *discordgo.Session, m *discordgo.MessageCreate, args []string) error {
	embed := c.buildHelpEmbed()
	_, err := s.ChannelMessageSendEmbed(m.ChannelID, embed)
	return err
}

func (c *HelpCommand) ExecuteSlash(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	embed := c.buildHelpEmbed()
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
		},
	})
}

func (c *HelpCommand) SlashDefinition() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Name:        c.Name(),
		Description: c.Description(),
	}
}

func (c *HelpCommand) buildHelpEmbed() *discordgo.MessageEmbed {
	embed := &discordgo.MessageEmbed{
		Title:       "📋 コマンド一覧",
		Description: "利用可能なコマンドの一部を表示しています。詳細は各コマンドを実行してください。",
		Color:       0x5865F2, // Discord Blurple
		Fields:      []*discordgo.MessageEmbedField{},
	}

	// コマンドを名前順で追加
	cmds := make([]Command, 0, len(c.registry.All()))
	for _, cmd := range c.registry.All() {
		cmds = append(cmds, cmd)
	}
	sort.Slice(cmds, func(i, j int) bool {
		return cmds[i].Name() < cmds[j].Name()
	})

	const maxFields = 25
	if len(cmds) > maxFields {
		cmds = cmds[:maxFields]
	}
	for _, cmd := range cmds {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "🔹 " + cmd.Name(),
			Value:  cmd.Description(),
			Inline: false,
		})
	}

	embed.Footer = &discordgo.MessageEmbedFooter{
		Text: "テキストコマンドは ! プレフィックスを使用してください。スラッシュコマンドも利用可能です。",
	}

	return embed
}
