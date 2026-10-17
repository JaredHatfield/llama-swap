package config

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const poolsConfig = `routing:
  router:
    use: capacity
    settings:
      capacity:
        pools:
          - name: ram
            total: 100
          - name: gpu
            total: 10
models:
  a:
    cmd: echo a
    proxy: http://localhost:8080
    capacity:
      evict_cost: 10
      pools:
        - pool: ram
          usage: 20
        - pool: gpu
          usage: 6
  b:
    cmd: echo b
    proxy: http://localhost:8081
    capacity:
      pools:
        - pool: ram
          usage: 30
`

func TestConfig_CapacityPools(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(poolsConfig))
	require.NoError(t, err)
	c := cfg.Routing.Router.Settings.Capacity
	require.Equal(t, []int64{100, 10}, c.Totals())
	require.Equal(t, []int64{20, 6}, c.UsageVector(cfg.Models["a"].Capacity))
	require.Equal(t, []int64{30, 0}, c.UsageVector(cfg.Models["b"].Capacity))
	require.Equal(t, int64(1), cfg.Models["b"].Capacity.EvictionCost())
	dir := t.TempDir()
	writeYAML(t, dir, "pools.yaml", poolsConfig)
	merged, err := LoadConfigSources("", dir)
	require.NoError(t, err)
	require.Equal(t, c, merged.Routing.Router.Settings.Capacity)
}

func TestConfig_CapacityPoolsValidation(t *testing.T) {
	for _, tc := range []struct{ name, from, to, want string }{
		{"mixed router", "        pools:", "        total: 100\n        pools:", "cannot be combined"},
		{"mixed zero router", "        pools:", "        total: 0\n        pools:", "cannot be combined"},
		{"duplicate declaration", "name: gpu", "name: ram", "duplicate pool"},
		{"unknown reference", "pool: gpu", "pool: typo", "unknown pool"},
		{"duplicate reference", "pool: gpu", "pool: ram", "duplicate pool"},
		{"blank name", "name: gpu", "name: ' '", "nonempty string"},
		{"numeric name", "name: gpu", "name: 42", "nonempty string"},
		{"zero total", "total: 10\n", "total: 0\n", "positive integer"},
		{"fraction total", "total: 10\n", "total: 10.5\n", "must be an integer"},
		{"zero usage", "usage: 6", "usage: 0", "positive integer"},
		{"fraction usage", "usage: 6", "usage: 6.5", "must be an integer"},
		{"oversized", "usage: 6", "usage: 11", "exceeds total"},
		{"unknown field", "usage: 6", "usage: 6\n          typo: 1", "unknown capacity setting"},
		{"mixed model", "      evict_cost: 10", "      usage: 1\n      evict_cost: 10", "cannot be combined"},
		{"empty router pools", "        pools:\n          - name: ram\n            total: 100\n          - name: gpu\n            total: 10", "        pools: []", "nonempty list"},
		{"empty model pools", "      pools:\n        - pool: ram\n          usage: 30", "      pools: []", "nonempty list"},
		{"null model pools", "      pools:\n        - pool: ram\n          usage: 30", "      pools: null", "nonempty list"},
		{"scalar pool entry", "        - pool: ram\n          usage: 30", "        - 42", "mapping"},
		{"null usage", "usage: 6", "usage: null", "must be an integer"},
		{"overflow usage", "usage: 6", "usage: 9223372036854775808", "cannot unmarshal"},
		{"scalar in named mode", "      pools:\n        - pool: ram\n          usage: 30", "      usage: 30", "named pools require"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfigFromReader(strings.NewReader(strings.Replace(poolsConfig, tc.from, tc.to, 1)))
			require.ErrorContains(t, err, tc.want)
		})
	}
	_, err := LoadConfigFromReader(strings.NewReader(strings.Replace(poolsConfig, "        pools:\n          - name: ram\n            total: 100\n          - name: gpu\n            total: 10", "        total: 100", 1)))
	require.ErrorContains(t, err, "requires named router pools")
}

func TestConfig_CapacityExclusiveDefaults(t *testing.T) {
	for _, declaration := range []string{"", "    capacity: null\n", "    capacity: {}\n", "    capacity: {evict_cost: 7}\n"} {
		for _, routing := range []string{capacityRouting, strings.Split(poolsConfig, "models:")[0]} {
			cfg, err := LoadConfigFromReader(strings.NewReader(routing + "models:\n  a:\n    cmd: echo a\n    proxy: http://localhost:8080\n" + declaration))
			require.NoError(t, err)
			require.True(t, cfg.Models["a"].Capacity.Exclusive())
		}
	}
}

func TestConfig_CapacityPoolsSpillover(t *testing.T) {
	selector := "selectors:\n  spill:\n    strategy: spillover\n    targets: [a, b]\n"
	_, err := LoadConfigFromReader(strings.NewReader(poolsConfig + selector))
	require.NoError(t, err)
	bothGPU := strings.Replace(poolsConfig, "pool: ram\n          usage: 30", "pool: gpu\n          usage: 5", 1)
	_, err = LoadConfigFromReader(strings.NewReader(bothGPU + selector))
	require.ErrorContains(t, err, "spillover targets exceed capacity")
	exclusive := strings.Replace(poolsConfig, "    capacity:\n      pools:\n        - pool: ram\n          usage: 30\n", "", 1)
	_, err = LoadConfigFromReader(strings.NewReader(exclusive + selector))
	require.ErrorContains(t, err, "exclusive model")
}

// Load the actual complete examples, so routing documentation cannot silently
// retain syntax rejected by the current config loader.
func TestConfig_CapacityGuideExamples(t *testing.T) {
	data, err := os.ReadFile("../../docs/kb/guides/routing/groups-and-matrix.md")
	require.NoError(t, err)
	_, section, found := strings.Cut(string(data), "## `capacity`")
	require.True(t, found, "capacity section must exist")
	section = strings.SplitN(section, "## Which one?", 2)[0]
	examples := strings.Split(section, "```yaml\n")[1:]
	require.Len(t, examples, 2, "keep named pools and single-budget compatibility examples")
	for i, example := range examples {
		input := strings.SplitN(example, "```", 2)[0]
		cfg, err := LoadConfigFromReader(strings.NewReader(input))
		require.NoError(t, err, "example %d", i+1)
		require.Equal(t, "capacity", cfg.Routing.Router.Use)
		// The guide says that all models in either example fit together.
		c := cfg.Routing.Router.Settings.Capacity
		totals := c.Totals()
		used := make([]int64, len(totals))
		for _, model := range cfg.Models {
			require.False(t, model.Capacity.Exclusive())
			for p, amount := range c.UsageVector(model.Capacity) {
				used[p] += amount
			}
		}
		for p, total := range totals {
			require.LessOrEqual(t, used[p], total)
		}
	}
}
