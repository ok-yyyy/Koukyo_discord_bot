package eastereggs

import (
	"math/rand"
	"sync"
	"time"
)

type randomRegistry struct {
	mu  sync.Mutex
	rnd *rand.Rand
}

var registry = randomRegistry{
	rnd: rand.New(rand.NewSource(time.Now().UnixNano())),
}

func ShouldTriggerChance(chancePercent float64) bool {
	registry.mu.Lock()
	ok := shouldTriggerChanceLocked(chancePercent)
	registry.mu.Unlock()
	return ok
}

func WeightedChoice(choices []WeightedReply) (string, bool) {
	registry.mu.Lock()
	reply, ok := weightedChoiceLocked(choices)
	registry.mu.Unlock()
	return reply, ok
}

type WeightedReply struct {
	Reply         string  `json:"reply"`
	WeightPercent float64 `json:"weight"`
}

func shouldTriggerChanceLocked(chancePercent float64) bool {
	switch {
	case chancePercent >= 100:
		return true
	case chancePercent <= 0:
		return false
	default:
		return registry.rnd.Float64()*100 < chancePercent
	}
}

func weightedChoiceLocked(choices []WeightedReply) (string, bool) {
	total := 0.0
	for _, choice := range choices {
		if choice.Reply == "" || choice.WeightPercent <= 0 {
			continue
		}
		total += choice.WeightPercent
	}
	if total <= 0 {
		return "", false
	}
	target := registry.rnd.Float64() * total
	acc := 0.0
	for _, choice := range choices {
		if choice.Reply == "" || choice.WeightPercent <= 0 {
			continue
		}
		acc += choice.WeightPercent
		if target < acc {
			return choice.Reply, true
		}
	}
	return "", false
}
