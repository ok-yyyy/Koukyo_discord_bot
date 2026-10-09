package utils

const (
	// WplaceRegionsPerEdge 1辺あたりのRegion数（全体は512x512）
	WplaceRegionsPerEdge = 512
	// WplaceTilesPerRegion 1Regionの1辺あたりのタイル数
	WplaceTilesPerRegion = 4
)

// RegionTileRange Regionが占めるタイル範囲（両端を含む）を返す
func RegionTileRange(regionX, regionY int) (minTile, maxTile [2]int) {
	minTile = [2]int{regionX * WplaceTilesPerRegion, regionY * WplaceTilesPerRegion}
	maxTile = [2]int{minTile[0] + WplaceTilesPerRegion - 1, minTile[1] + WplaceTilesPerRegion - 1}
	return minTile, maxTile
}

// RegionCenterLngLat Regionの中心の経度緯度を返す
func RegionCenterLngLat(regionX, regionY int) *LngLat {
	half := WplaceTilesPerRegion / 2
	return TilePixelToLngLatFloat(regionX*WplaceTilesPerRegion+half, regionY*WplaceTilesPerRegion+half, 0, 0)
}
