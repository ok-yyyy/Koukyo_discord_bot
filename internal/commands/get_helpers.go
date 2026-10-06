package commands

import (
	"bytes"

	"github.com/bwmarrin/discordgo"
)

func respondGet(s *discordgo.Session, i *discordgo.InteractionCreate, msg string) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: msg,
		},
	})
}

func respondDeferred(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
}

func followupMessage(s *discordgo.Session, i *discordgo.InteractionCreate, msg string) error {
	_, err := s.FollowupMessageCreate(i.Interaction, false, &discordgo.WebhookParams{
		Content: msg,
	})
	return err
}

func sendImageFollowup(s *discordgo.Session, i *discordgo.InteractionCreate, imageData []byte, filename string, embed *discordgo.MessageEmbed) error {
	params := &discordgo.WebhookParams{
		Files: []*discordgo.File{
			{
				Name:        filename,
				ContentType: "image/png",
				Reader:      bytes.NewReader(imageData),
			},
		},
	}
	if embed != nil {
		params.Embeds = []*discordgo.MessageEmbed{embed}
	}
	_, err := s.FollowupMessageCreate(i.Interaction, false, params)
	return err
}
