package notifications

import (
	"fmt"
	"math"
)

// Tier 通知段階
type Tier int

const (
	TierNone Tier = iota
	Tier10        // 10%以上
	Tier20        // 20%以上
	Tier30        // 30%以上
	Tier40        // 40%以上
	Tier50        // 50%以上（メンション閾値）
	Tier60        // 60%以上
	Tier70        // 70%以上
	Tier80        // 80%以上
	Tier90        // 90%以上
	Tier100       // 100%以上
)

// metricLabel 通知文面で使う指標名
const metricLabel = "差分率"

// calculateTier 差分率からTierを計算
func calculateTier(diffValue, threshold float64) Tier {
	if diffValue < threshold {
		return TierNone
	}
	if diffValue >= 100 {
		return Tier100
	}
	if diffValue >= 90 {
		return Tier90
	}
	if diffValue >= 80 {
		return Tier80
	}
	if diffValue >= 70 {
		return Tier70
	}
	if diffValue >= 60 {
		return Tier60
	}
	if diffValue >= 50 {
		return Tier50
	}
	if diffValue >= 40 {
		return Tier40
	}
	if diffValue >= 30 {
		return Tier30
	}
	if diffValue >= 20 {
		return Tier20
	}
	return Tier10
}

func isZeroDiff(value float64) bool {
	const zeroDiffEpsilon = 0.005
	return math.Abs(value) <= zeroDiffEpsilon
}

// getTierColor Tierに応じた色を取得
func getTierColor(tier Tier) int {
	switch tier {
	case Tier100:
		return 0x7F0000 // 濃い赤
	case Tier90:
		return 0xB22222 // ファイアブリック
	case Tier80:
		return 0xDC143C // クリムゾン
	case Tier70:
		return 0xFF3030 // 明るい赤
	case Tier60:
		return 0xFF4500 // オレンジレッド
	case Tier50:
		return 0xFF0000 // 赤
	case Tier40:
		return 0xFF4500 // オレンジレッド
	case Tier30:
		return 0xFFA500 // オレンジ
	case Tier20:
		return 0xFFD700 // ゴールド
	case Tier10:
		return 0xFFFF00 // 黄色
	default:
		return 0x808080 // グレー
	}
}

func tierRangeLabel(tier Tier, threshold float64) string {
	switch tier {
	case Tier100:
		return "100%台"
	case Tier90:
		return "90%台"
	case Tier80:
		return "80%台"
	case Tier70:
		return "70%台"
	case Tier60:
		return "60%台"
	case Tier50:
		return "50%以上"
	case Tier40:
		return "40%台"
	case Tier30:
		return "30%台"
	case Tier20:
		return "20%台"
	case Tier10:
		return "10%台"
	default:
		return fmt.Sprintf("%.0f%%未満", threshold)
	}
}
