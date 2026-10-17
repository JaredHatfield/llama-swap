package router

import (
	"fmt"
	"io"
	"math/rand"
	"slices"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/stretchr/testify/require"
)

func poolModel(cost int64, amounts ...int64) config.ModelConfig {
	mc := &config.ModelCapacityConfig{EvictCost: cost}
	for p, amount := range amounts {
		if amount > 0 {
			mc.Pools = append(mc.Pools, config.ModelCapacityPoolConfig{Pool: fmt.Sprint(p), Usage: amount})
		}
	}
	return config.ModelConfig{Capacity: mc}
}

func poolPlanner(models map[string]config.ModelConfig) *capacitySwapper {
	return &capacitySwapper{capacity: &config.CapacityConfig{Pools: []config.CapacityPoolConfig{{Name: "0", Total: 10}, {Name: "1", Total: 10}, {Name: "2", Total: 10}}}, models: models, logger: logmon.NewWriter(io.Discard)}
}

func TestCapacity_Pools(t *testing.T) {
	for _, tc := range []struct {
		name          string
		models        map[string]config.ModelConfig
		running, want []string
	}{
		{"disjoint", map[string]config.ModelConfig{"a": poolModel(1, 10), "x": poolModel(1, 0, 10)}, []string{"a"}, nil},
		{"shared cost counted once", map[string]config.ModelConfig{"a": poolModel(3, 5, 5), "b": poolModel(2, 5), "c": poolModel(2, 0, 5), "x": poolModel(1, 5, 5)}, []string{"c", "a", "b"}, []string{"a"}},
		{"independent deficits", map[string]config.ModelConfig{"a": poolModel(1, 10), "b": poolModel(1, 0, 10), "c": poolModel(1, 0, 0, 10), "x": poolModel(1, 1, 1)}, []string{"a", "b", "c"}, []string{"a", "b"}},
		{"exclusive target", map[string]config.ModelConfig{"a": poolModel(1, 10), "b": poolModel(1, 0, 10), "x": {}}, []string{"x", "b", "a", "a"}, []string{"a", "b"}},
		{"exclusive resident", map[string]config.ModelConfig{"a": {}, "x": poolModel(1, 0, 10)}, []string{"a"}, []string{"a"}},
		{"cost only exclusive resident", map[string]config.ModelConfig{"a": {Capacity: &config.ModelCapacityConfig{EvictCost: 100}}, "x": poolModel(1, 10)}, []string{"a"}, []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := poolPlanner(tc.models)
			require.NoError(t, config.ValidateCapacity(s.capacity, tc.models))
			require.Equal(t, tc.want, s.EvictionFor("x", tc.running))
		})
	}
}

// The oracle sums the retained models directly, independently of planner
// deficits, pruning and greedy seeding. Check cost, count and lexical ties.
func TestCapacity_PoolsExhaustiveOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(319))
	for trial := 0; trial < 400; trial++ {
		models := map[string]config.ModelConfig{}
		var running []string
		amounts := map[string][]int64{}
		costs := map[string]int64{}
		for i := 0; i < 9; i++ {
			id := fmt.Sprintf("m%d", i)
			if i == 8 {
				id = "x"
			} else {
				running = append(running, id)
			}
			v := []int64{int64(rng.Intn(5)), int64(rng.Intn(5)), int64(rng.Intn(5))}
			if v[0]+v[1]+v[2] == 0 {
				v[0] = 1
			}
			cost := int64(1 + rng.Intn(7))
			amounts[id] = v
			costs[id] = cost
			models[id] = poolModel(cost, v...)
		}
		s := poolPlanner(models)
		require.NoError(t, config.ValidateCapacity(s.capacity, models))
		var want []string
		var bestCost int64
		found := false
		for mask := 0; mask < 1<<len(running); mask++ {
			used := slices.Clone(amounts["x"])
			var evict []string
			var cost int64
			for i, id := range running {
				if mask&(1<<i) != 0 {
					evict = append(evict, id)
					cost += costs[id]
				} else {
					for p, a := range amounts[id] {
						used[p] += a
					}
				}
			}
			if used[0] > 10 || used[1] > 10 || used[2] > 10 {
				continue
			}
			if !found || cost < bestCost || cost == bestCost && (len(evict) < len(want) || len(evict) == len(want) && slices.Compare(evict, want) < 0) {
				want, bestCost, found = evict, cost, true
			}
		}
		require.Equal(t, want, s.EvictionFor("x", running), "trial %d", trial)
		require.False(t, s.searchLimited)
		// A one-node budget must still yield a deterministic feasible plan.
		s.searchLimit = 1
		s.lastValid = false
		actual := s.EvictionFor("x", running)
		used := slices.Clone(amounts["x"])
		for _, id := range running {
			if !slices.Contains(actual, id) {
				for p, a := range amounts[id] {
					used[p] += a
				}
			}
		}
		for _, a := range used {
			require.LessOrEqual(t, a, int64(10), "fallback trial %d", trial)
		}
		require.LessOrEqual(t, s.searchNodes, 1)
		s.lastValid = false
		slices.Reverse(running)
		require.Equal(t, actual, s.EvictionFor("x", running))
	}
}

func BenchmarkCapacity_Pools(b *testing.B) {
	for _, n := range []int{8, 24, 64} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			models := map[string]config.ModelConfig{"x": poolModel(1, 5, 5, 5)}
			running := make([]string, n)
			for i := range running {
				running[i] = fmt.Sprintf("m%03d", i)
				models[running[i]] = poolModel(int64(1+i%7), 1, 1, 1)
			}
			s := poolPlanner(models)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.evictionFor("x", running)
			}
		})
	}
}
