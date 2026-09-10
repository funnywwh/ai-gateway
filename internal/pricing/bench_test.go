package pricing

import (
	"testing"
	"time"
)

func BenchmarkEvaluate(b *testing.B) {
	set, err := ParseRuleSet(deepseekCost)
	if err != nil {
		b.Fatal(err)
	}
	at := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	dimensions := map[string]int64{"input_cache_hit": 2000, "input_cache_miss": 3000, "output": 5000}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		result := Evaluate(Input{Cost: set, At: at, Dimensions: dimensions})
		if result.CostMicros == 0 {
			b.Fatal("no cost")
		}
	}
}

func BenchmarkEvaluateParallel(b *testing.B) {
	set, err := ParseRuleSet(deepseekCost)
	if err != nil {
		b.Fatal(err)
	}
	at := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	dimensions := map[string]int64{"input_cache_hit": 2000, "input_cache_miss": 3000, "output": 5000}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if Evaluate(Input{Cost: set, At: at, Dimensions: dimensions}).CostMicros == 0 {
				b.Error("no cost")
				return
			}
		}
	})
}
