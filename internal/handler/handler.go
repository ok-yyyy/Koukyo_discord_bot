package handler

import (
	"Koukyo_discord_bot/internal/commands"
	"Koukyo_discord_bot/internal/config"
	"Koukyo_discord_bot/internal/models"
	"Koukyo_discord_bot/internal/monitor"
	"Koukyo_discord_bot/internal/notifications"
	"Koukyo_discord_bot/internal/utils" // これを追加
	"sync"
	"time"
)

type Handler struct {
	registry *commands.Registry
	prefix   string
	botInfo  *models.BotInfo
	monitor  *monitor.Monitor
	settings *config.SettingsManager
	notifier *notifications.Notifier
	limiter  *utils.RateLimiter // これを追加
	dataDir  string

	readyMu          sync.Mutex
	readyInitialized bool
	lastReadyAt      time.Time
}

func NewHandler(prefix string, botInfo *models.BotInfo, mon *monitor.Monitor, settingsManager *config.SettingsManager, notifier *notifications.Notifier, limiter *utils.RateLimiter, activityLimiter *utils.RateLimiter, dataDir string) *Handler { // limiter 引数を追加
	registry := commands.NewRegistry()

	// すべてのコマンドを配列で一元管理
	var commandsList []commands.Command
	commandsList = append(commandsList,
		&commands.PingCommand{},
		commands.NewInfoCommand(botInfo),
		commands.NewExplanationCommand(),
		commands.NewStatusCommand(botInfo, notifier),
		commands.NewNowCommand(mon),
		commands.NewTimeCommand(),
		commands.NewConvertCommand(),
		commands.NewMeCommand(dataDir, activityLimiter),
		commands.NewAchievementsCommand(dataDir),
		commands.NewSettingsCommand(settingsManager, notifier), // settingsManager を渡す
		commands.NewNotificationCommand(settingsManager),
		commands.NewProgressChannelCommand(settingsManager),
		commands.NewAchievementChannelCommand(settingsManager),
		commands.NewDMCommand(settingsManager),
		commands.NewGetCommand(limiter), // limiter を渡すように変更
		commands.NewPaintCommand(notifier),
		commands.NewRegionMapCommand(),
		commands.NewUserActivityCommand(dataDir),
		commands.NewFixUserCommand(dataDir),
		commands.NewGrfUserCommand(dataDir),
	)
	if mon != nil {
		commandsList = append(commandsList,
			commands.NewGraphCommand(mon, dataDir),
			commands.NewPredictCommand(mon),
			commands.NewTimelapseCommand(mon),
			commands.NewHeatmapCommand(mon),
		)
	}
	// HelpCommandは最後に追加し、registryを渡す
	helpCmd := commands.NewHelpCommand(registry)
	commandsList = append(commandsList, helpCmd)

	// 配列から一括登録
	for _, cmd := range commandsList {
		registry.Register(cmd)
	}

	return &Handler{
		registry: registry,
		prefix:   prefix,
		botInfo:  botInfo,
		monitor:  mon,
		settings: settingsManager, // settingsManager を使用
		notifier: notifier,
		limiter:  limiter, // これを追加
		dataDir:  dataDir,
	}
}
