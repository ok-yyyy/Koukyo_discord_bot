package notifications

import (
	"Koukyo_discord_bot/internal/activity"
	"Koukyo_discord_bot/internal/utils"
	"bytes"
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"
)

func buildUserNotifyEmbed(title string, user activity.UserActivity, isVandal bool) (*discordgo.MessageEmbed, *discordgo.File) {
	name := utils.FormatUserDisplayName(user.Name, user.ID)
	alliance := user.AllianceName
	if alliance == "" {
		alliance = "-"
	}
	count := user.VandalCount
	color := 0xE74C3C
	if !isVandal {
		count = user.RestoredCount
		color = 0x2ECC71
	}
	lastSeen := user.LastSeen
	if lastSeen == "" {
		lastSeen = "-"
	}
	discordName := user.Discord
	if discordName == "" {
		discordName = "-"
	}
	discordID := user.DiscordID
	if discordID == "" {
		discordID = "-"
	}
	embed := &discordgo.MessageEmbed{
		Title: title,
		Color: color,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "ユーザー", Value: name, Inline: true},
			{Name: "同盟", Value: alliance, Inline: true},
			{Name: "累計", Value: fmt.Sprintf("%d", count), Inline: true},
			{Name: "Discord", Value: discordName, Inline: true},
			{Name: "Discord ID", Value: discordID, Inline: true},
			{Name: "最終観測", Value: lastSeen, Inline: false},
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	file := utils.DecodePictureDataURL(user.Picture)
	if file == nil && user.ID != "" {
		if data, err := utils.BuildIdenticonPNG(user.ID, utils.DefaultIdenticonSize); err == nil {
			file = &discordgo.File{
				Name:        "user_identicon.png",
				ContentType: "image/png",
				Reader:      bytes.NewReader(data),
			}
		}
	}
	if file != nil {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{
			URL: "attachment://" + file.Name,
		}
	}
	return embed, file
}
