package notifications

import (
	"Koukyo_discord_bot/internal/activity"
	"Koukyo_discord_bot/internal/embeds"
	"Koukyo_discord_bot/internal/utils"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

func (n *Notifier) startDailyRankingLoop() {
	go func() {
		jst := time.FixedZone("JST", 9*3600)
		for {
			now := time.Now().In(jst)
			next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, jst)
			sleep := time.Until(next)
			if sleep < time.Second {
				sleep = time.Second
			}
			timer := time.NewTimer(sleep)
			<-timer.C

			reportTime := next.Add(-time.Second).In(jst)
			reportDate := reportTime.Format("2006-01-02")
			if reportDate == n.lastDailyReportDate {
				continue
			}
			if err := n.sendDailyRankingReport(reportTime); err != nil {
				log.Printf("Failed to send daily ranking report: %v", err)
				continue
			}
			n.lastDailyReportDate = reportDate
		}
	}()
}

type rankingEntry struct {
	ID         string
	Name       string
	AllianceID int
	Alliance   string
	Count      int
}

func (n *Notifier) sendDailyRankingReport(reportTime time.Time) error {
	if n.dataDir == "" {
		return fmt.Errorf("dataDir is empty")
	}
	path := filepath.Join(n.dataDir, "user_activity.json")
	entries, err := readUserActivityWithRetry(path, 3, 100*time.Millisecond)
	if err != nil {
		return err
	}

	jst := time.FixedZone("JST", 9*3600)
	dateKey := reportTime.In(jst).Format("2006-01-02")

	vandals := buildRanking(entries, dateKey, true)
	restores := buildRanking(entries, dateKey, false)
	activities := buildActivityRanking(entries, dateKey)

	vandalText := formatRanking(vandals)
	restoreText := formatRanking(restores)
	activityText := formatActivityRanking(activities)
	summaryText := n.buildDailyDiffSummary(dateKey, jst)

	titleDate := reportTime.In(jst).Format("2006-01-02 (JST)")
	peakLiveImage, peakDiffImage, _, _, peakOK := n.monitor.State.GetDailyPeakImages(dateKey)
	peakAttachmentData, peakAttachmentName := buildPeakImageAttachmentData(peakLiveImage, peakDiffImage, peakOK)
	embed := buildDailyRankingEmbed(titleDate, summaryText, vandalText, restoreText, activityText, "")

	for _, guild := range n.session.State.Guilds {
		gs := n.settings.GetGuildSettings(guild.ID)
		if !gs.AutoNotifyEnabled || gs.NotificationChannel == nil {
			continue
		}
		peakFiles := buildPeakFilesForSend(peakAttachmentData, peakAttachmentName)
		msg, err := n.session.ChannelMessageSendComplex(*gs.NotificationChannel, &discordgo.MessageSend{
			Embeds: []*discordgo.MessageEmbed{embed},
			Files:  peakFiles,
		})
		if err != nil {
			log.Printf("Failed to send daily ranking to guild %s: %v", guild.ID, err)
			continue
		}
		if peakAttachmentName != "" && len(msg.Attachments) > 0 {
			link := msg.Attachments[0].URL
			updated := buildDailyRankingEmbed(titleDate, summaryText, vandalText, restoreText, activityText, link)
			if _, err := n.session.ChannelMessageEditComplex(&discordgo.MessageEdit{
				ID:      msg.ID,
				Channel: msg.ChannelID,
				Embeds:  &[]*discordgo.MessageEmbed{updated},
			}); err != nil {
				log.Printf("Failed to update daily ranking link for guild %s: %v", guild.ID, err)
			}
		}
		log.Printf("Sent daily ranking to guild %s", guild.ID)
	}

	return nil
}

func readUserActivityWithRetry(path string, attempts int, delay time.Duration) (map[string]*activity.UserActivity, error) {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		data, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
		} else {
			var entries map[string]*activity.UserActivity
			if err := json.Unmarshal(data, &entries); err != nil {
				lastErr = err
			} else {
				return entries, nil
			}
		}
		time.Sleep(delay)
	}
	return nil, lastErr
}

func buildDailyRankingEmbed(titleDate, summaryText, vandalText, restoreText, activityText, peakLink string) *discordgo.MessageEmbed {
	fields := []*discordgo.MessageEmbedField{
		{
			Name:   "📈 日次サマリ",
			Value:  summaryText,
			Inline: false,
		},
		{
			Name:   "🚨 荒らしランキング",
			Value:  vandalText,
			Inline: false,
		},
		{
			Name:   "🛠️ 修復ランキング",
			Value:  restoreText,
			Inline: false,
		},
		{
			Name:   "🧮 総合ランキング (修復 - 荒らし)",
			Value:  activityText,
			Inline: false,
		},
	}
	if peakLink != "" {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "🖼️ ピーク差分画像",
			Value:  peakLink,
			Inline: false,
		})
	}
	return &discordgo.MessageEmbed{
		Title:       "📊 日次ランキング",
		Description: fmt.Sprintf("%s の荒らし/修復/総合ランキング", titleDate),
		Color:       0x1E90FF,
		Fields:      fields,
		Timestamp:   time.Now().Format(time.RFC3339),
		Footer: &discordgo.MessageEmbedFooter{
			Text: "自動日次レポート",
		},
	}
}

func buildPeakImageAttachmentData(liveImg, diffImg []byte, ok bool) ([]byte, string) {
	if !ok || len(diffImg) == 0 {
		return nil, ""
	}

	if len(liveImg) > 0 {
		combined, err := embeds.CombineImages(liveImg, diffImg)
		if err == nil {
			data, readErr := io.ReadAll(combined)
			if readErr != nil {
				log.Printf("Failed to read combined daily peak image: %v", readErr)
			} else {
				name := "daily_peak_combined.png"
				return data, name
			}
		} else {
			log.Printf("Failed to combine daily peak images: %v", err)
		}
	}

	name := "daily_peak_diff.png"
	return append([]byte(nil), diffImg...), name
}

func buildPeakFilesForSend(attachmentData []byte, attachmentName string) []*discordgo.File {
	if len(attachmentData) == 0 || attachmentName == "" {
		return nil
	}
	return []*discordgo.File{{
		Name:        attachmentName,
		ContentType: "image/png",
		Reader:      bytes.NewReader(attachmentData),
	}}
}

func buildRanking(entries map[string]*activity.UserActivity, dateKey string, vandal bool) []rankingEntry {
	out := make([]rankingEntry, 0)
	for id, entry := range entries {
		var count int
		if vandal {
			count = entry.DailyVandalCounts[dateKey]
		} else {
			count = entry.DailyRestoredCounts[dateKey]
		}
		if count <= 0 {
			continue
		}
		name := entry.Name
		if name == "" {
			name = fmt.Sprintf("ID:%s", id)
		}
		out = append(out, rankingEntry{
			ID:         id,
			Name:       name,
			AllianceID: entry.AllianceID,
			Alliance:   entry.AllianceName,
			Count:      count,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count == out[j].Count {
			return out[i].Name < out[j].Name
		}
		return out[i].Count > out[j].Count
	})
	return out
}

func formatRanking(entries []rankingEntry) string {
	if len(entries) == 0 {
		return "該当なし"
	}
	limit := 10
	if len(entries) < limit {
		limit = len(entries)
	}
	lines := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		entry := entries[i]
		display := utils.FormatUserDisplayName(entry.Name, entry.ID)
		if entry.Alliance != "" {
			display = fmt.Sprintf("%s (%s)", display, entry.Alliance)
		}
		lines = append(lines, fmt.Sprintf("%d. %s | %d", i+1, display, entry.Count))
	}
	return strings.Join(lines, "\n")
}

func (n *Notifier) buildDailyDiffSummary(dateKey string, jst *time.Location) string {
	if n.monitor == nil || n.monitor.State == nil {
		return "監視データなし"
	}

	summary, ok := n.monitor.State.GetDailySummary(dateKey)
	if !ok {
		return strings.Join([]string{
			"最新差分率: N/A",
			"最大差分率: N/A",
			"最小差分率: N/A",
			"平均差分率: N/A",
			"記録数: 0",
		}, "\n")
	}
	overall := summary.Overall

	avgOverall := 0.0
	if overall.Count > 0 {
		avgOverall = overall.Sum / float64(overall.Count)
	}

	lines := []string{
		fmt.Sprintf("最新差分率: %s", formatPercent(overall.Latest, overall.Count > 0)),
		fmt.Sprintf("最大差分率: %s %s", formatPercent(overall.Max, overall.Count > 0), formatTimeJST(overall.PeakAt, overall.Count > 0, jst)),
		fmt.Sprintf("最小差分率: %s", formatPercent(overall.Min, overall.Count > 0)),
		fmt.Sprintf("平均差分率: %s", formatPercent(avgOverall, overall.Count > 0)),
		fmt.Sprintf("記録数: %d", overall.Count),
	}
	return strings.Join(lines, "\n")
}

func formatPercent(value float64, ok bool) string {
	if !ok {
		return "N/A"
	}
	return fmt.Sprintf("%.2f%%", value)
}

func formatTimeJST(t time.Time, ok bool, jst *time.Location) string {
	if !ok || t.IsZero() {
		return ""
	}
	return fmt.Sprintf("(%s)", t.In(jst).Format("15:04"))
}

func buildActivityRanking(entries map[string]*activity.UserActivity, dateKey string) []rankingEntry {
	out := make([]rankingEntry, 0)
	for id, entry := range entries {
		count := entry.DailyActivityScores[dateKey]
		if count == 0 {
			continue
		}
		name := entry.Name
		if name == "" {
			name = fmt.Sprintf("ID:%s", id)
		}
		out = append(out, rankingEntry{
			ID:         id,
			Name:       name,
			AllianceID: entry.AllianceID,
			Alliance:   entry.AllianceName,
			Count:      count,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count == out[j].Count {
			return out[i].Name < out[j].Name
		}
		return out[i].Count > out[j].Count
	})
	return out
}

func formatActivityRanking(entries []rankingEntry) string {
	if len(entries) == 0 {
		return "該当なし"
	}
	limit := 10
	if len(entries) < limit {
		limit = len(entries)
	}
	lines := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		entry := entries[i]
		display := utils.FormatUserDisplayName(entry.Name, entry.ID)
		if entry.Alliance != "" {
			display = fmt.Sprintf("%s (%s)", display, entry.Alliance)
		}
		lines = append(lines, fmt.Sprintf("%d. %s | %d", i+1, display, entry.Count))
	}
	return strings.Join(lines, "\n")
}
