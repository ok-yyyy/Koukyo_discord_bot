package commands

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"Koukyo_discord_bot/internal/utils"
)

// regionDatabaseJSONL はRegionデータベース（1行1RegionのJSON Lines、region_id順）。
//
//go:embed region_database.jsonl
var regionDatabaseJSONL []byte

var regionDBCache struct {
	once sync.Once
	data RegionDB
	err  error
}

type Region struct {
	RegionID     int    `json:"region_id"`
	Name         string `json:"name"`
	CountryID    int    `json:"country_id"`
	CityID       int    `json:"city_id"`
	RegionCoords [2]int `json:"region_coords"`
	// 以下は RegionCoords から計算する
	TileRange    RegionTileRange `json:"-"`
	CenterLatLng [2]float64      `json:"-"`
}

// RegionDB は "名前_国ID"（例: Tokyo#1_110）をキーにしたRegionの一覧
type RegionDB map[string]Region

type RegionTileRange struct {
	Min [2]int
	Max [2]int
}

// loadRegionDB は埋め込まれたRegionデータベースを返す。解析は初回呼び出し時に一度だけ行う。
func loadRegionDB() (RegionDB, error) {
	regionDBCache.once.Do(func() {
		regionDBCache.data, regionDBCache.err = parseRegionDB(regionDatabaseJSONL)
	})
	return regionDBCache.data, regionDBCache.err
}

func parseRegionDB(jsonl []byte) (RegionDB, error) {
	db := make(RegionDB, utils.WplaceRegionsPerEdge*utils.WplaceRegionsPerEdge)
	sc := bufio.NewScanner(bytes.NewReader(jsonl))
	for line := 1; sc.Scan(); line++ {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var reg Region
		if err := json.Unmarshal(sc.Bytes(), &reg); err != nil {
			return nil, fmt.Errorf("region database line %d: %w", line, err)
		}
		x, y := reg.RegionCoords[0], reg.RegionCoords[1]
		reg.TileRange.Min, reg.TileRange.Max = utils.RegionTileRange(x, y)
		center := utils.RegionCenterLngLat(x, y)
		reg.CenterLatLng = [2]float64{center.Lat, center.Lng}
		db[fmt.Sprintf("%s_%d", reg.Name, reg.CountryID)] = reg
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("region database: %w", err)
	}
	return db, nil
}

func findRegionByName(db RegionDB, name string) (Region, bool) {
	if reg, ok := db[name]; ok {
		return reg, true
	}
	for _, reg := range db {
		if strings.EqualFold(reg.Name, name) {
			return reg, true
		}
	}
	return Region{}, false
}
