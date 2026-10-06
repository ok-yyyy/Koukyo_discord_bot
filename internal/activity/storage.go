package activity

import (
	"Koukyo_discord_bot/internal/utils"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var userActivityFileMu sync.Mutex

func UpdateUserActivityMap(dataDir string, update func(map[string]*UserActivity) error) error {
	if dataDir == "" {
		return fmt.Errorf("dataDir is empty")
	}
	path := filepath.Join(dataDir, "user_activity.json")
	userActivityFileMu.Lock()
	defer userActivityFileMu.Unlock()

	raw, err := loadUserActivityMapUnlocked(path)
	if err != nil {
		return err
	}
	if raw == nil {
		raw = make(map[string]*UserActivity)
	}
	if err := update(raw); err != nil {
		return err
	}
	return saveUserActivityMapUnlocked(path, raw)
}

func saveUserActivityMap(dataDir string, raw map[string]*UserActivity) error {
	if dataDir == "" {
		return fmt.Errorf("dataDir is empty")
	}
	path := filepath.Join(dataDir, "user_activity.json")
	userActivityFileMu.Lock()
	defer userActivityFileMu.Unlock()
	return saveUserActivityMapUnlocked(path, raw)
}

func loadUserActivityMapUnlocked(path string) (map[string]*UserActivity, error) {
	var raw map[string]*UserActivity
	_, err := utils.ReadJSONFileWithBackup(path, &raw)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]*UserActivity{}, nil
		}
		return nil, err
	}
	if raw == nil {
		return map[string]*UserActivity{}, nil
	}
	return raw, nil
}

func saveUserActivityMapUnlocked(path string, raw map[string]*UserActivity) error {
	payload, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return utils.WriteFileAtomic(path, payload)
}
