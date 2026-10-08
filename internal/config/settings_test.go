package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsManagerCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "settings.json")
	sm := NewSettingsManager(configPath)

	sm.Close()
	sm.Close()
}

func TestSettingsManagerCloseFlushesDirtyState(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "settings.json")
	sm := NewSettingsManager(configPath)

	channelID := "1234567890"
	settings := DefaultGuildSettings
	settings.NotificationChannel = &channelID
	sm.SetGuildSettings("guild-1", settings)

	sm.Close()

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read settings file: %v", err)
	}

	var saved map[string]GuildSettings
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("failed to parse saved settings: %v", err)
	}

	got, ok := saved["guild-1"]
	if !ok {
		t.Fatalf("guild settings were not persisted")
	}
	if got.NotificationChannel == nil || *got.NotificationChannel != channelID {
		t.Fatalf("notification channel not persisted: %+v", got.NotificationChannel)
	}
}

func TestSettingsManagerLoadsDefaultsForLegacySettings(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(configPath, []byte(`{"guild-1":{"notification_channel":"123"}}`), 0644); err != nil {
		t.Fatalf("failed to seed legacy settings: %v", err)
	}

	sm := NewSettingsManager(configPath)
	defer sm.Close()

	got := sm.GetGuildSettings("guild-1")
	if got.NotificationChannel == nil || *got.NotificationChannel != "123" {
		t.Fatalf("notification channel not loaded: %+v", got.NotificationChannel)
	}
	if !got.AutoNotifyEnabled {
		t.Fatalf("expected auto notify default to be restored")
	}
	if got.NotificationThreshold != DefaultGuildSettings.NotificationThreshold {
		t.Fatalf("unexpected notification threshold: %v", got.NotificationThreshold)
	}
	if got.MentionThreshold != DefaultGuildSettings.MentionThreshold {
		t.Fatalf("unexpected mention threshold: %v", got.MentionThreshold)
	}
}

func TestSettingsManagerLoadsUserDMWithoutSettingsFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "user_dm.json"), []byte(`{"user-1":true}`), 0644); err != nil {
		t.Fatalf("failed to seed user dm file: %v", err)
	}

	sm := NewSettingsManager(filepath.Join(dir, "settings.json"))
	defer sm.Close()

	if !sm.GetUserDMEnabled("user-1") {
		t.Fatalf("expected user dm setting to load without settings.json")
	}
}
