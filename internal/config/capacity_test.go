package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const capacityModels = `models:
  a:
    cmd: echo a
    proxy: http://localhost:8080
    aliases: [alias-a]
    capacity: {usage: 4, evict_cost: 1}
  b:
    cmd: echo b
    proxy: http://localhost:8081
    capacity: {usage: 6, evict_cost: 10}
`
const capacityRouting = `routing:
  router:
    use: capacity
    settings:
      capacity:
        total: 10
`

func TestConfig_Capacity(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(capacityModels + capacityRouting))
	require.NoError(t, err)
	require.Equal(t, "capacity", cfg.Routing.Router.Use)
	require.Equal(t, int64(10), cfg.Routing.Router.Settings.Capacity.Total)
	require.Equal(t, int64(4), cfg.Models["a"].Capacity.Usage)
	require.Equal(t, int64(10), cfg.Models["b"].Capacity.EvictCost)
	require.Empty(t, cfg.Groups)
	require.Empty(t, cfg.Routing.Router.Settings.Groups)
	require.Nil(t, cfg.Matrix)
	require.Nil(t, cfg.Routing.Router.Settings.Matrix)
	cfg, err = LoadConfigFromReader(strings.NewReader(strings.Replace(capacityModels+capacityRouting, ", evict_cost: 1", "", 1)))
	require.NoError(t, err)
	require.Equal(t, int64(1), cfg.Models["a"].Capacity.EvictCost)
}

func TestConfig_CapacityValidation(t *testing.T) {
	for _, tc := range []struct{ name, from, to, want string }{
		{"zero total", "total: 10", "total: 0", "total must be a positive integer"},
		{"negative total", "total: 10", "total: -1", "total must be a positive integer"},
		{"missing total", "total: 10", "{}", "total must be a positive integer"},
		{"fraction total", "total: 10", "total: 10.5", "total must be an integer"},
		{"nonfinite total", "total: 10", "total: .inf", "total must be an integer"},
		{"overflow total", "total: 10", "total: 9223372036854775808", "cannot unmarshal"},
		{"zero usage", "usage: 4", "usage: 0", "usage must be a positive integer"},
		{"negative usage", "usage: 4", "usage: -1", "usage must be a positive integer"},
		{"fraction usage", "usage: 4", "usage: 4.5", "usage must be an integer"},
		{"oversized model", "usage: 4", "usage: 11", "exceeds total"},
		{"negative cost", "evict_cost: 1}", "evict_cost: -1}", "evict_cost must be a positive integer"},
		{"zero cost", "evict_cost: 1}", "evict_cost: 0}", "evict_cost must be a positive integer"},
		{"fraction cost", "evict_cost: 1}", "evict_cost: 1.5}", "evict_cost must be an integer"},
		{"unknown model setting", "evict_cost: 1}", "eviction_priority: 1}", "unknown capacity setting"},
		{"unknown router setting", "total: 10", "budget: 10", "unknown capacity setting"},
		{"old router name", "use: capacity", "use: resource", "requires use: capacity"},
		{"old cost map", "total: 10", "total: 10\n        evict_costs: {a: 1}", "unknown capacity setting"},
		{"no settings", "    settings:\n      capacity:\n        total: 10\n", "", "requires use: capacity"},
		{"not selected", "use: capacity", "use: group", "requires use: capacity"},
		{"implicit group", "    use: capacity\n", "", "requires use: capacity"},
		{"group settings", "    settings:\n", "    settings:\n      groups: {}\n", "cannot be combined"},
		{"matrix settings", "    settings:\n", "    settings:\n      matrix: {sets: {s: a}}\n", "cannot be combined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfigFromReader(strings.NewReader(strings.Replace(capacityModels+capacityRouting, tc.from, tc.to, 1)))
			require.ErrorContains(t, err, tc.want)
		})
	}
	for _, legacy := range []string{"groups: {g: {members: [a]}}", "matrix: {sets: {s: a}}"} {
		_, err := LoadConfigFromReader(strings.NewReader(capacityModels + capacityRouting + legacy))
		require.ErrorContains(t, err, "legacy top-level")
	}
	_, err := LoadConfigFromReader(strings.NewReader(capacityModels + capacityRouting + "groups: {}"))
	require.ErrorContains(t, err, "cannot be combined")
	err = ValidateCapacity(&CapacityConfig{Total: 9223372036854775807}, map[string]ModelConfig{
		"a": {Capacity: &ModelCapacityConfig{Usage: 9223372036854775807, EvictCost: 1}}, "b": {Capacity: &ModelCapacityConfig{Usage: 1, EvictCost: 1}},
	})
	require.ErrorContains(t, err, "sum of capacity.usage")
	// Cost sums are bounded too, so the planner's score cannot overflow.
	_, err = LoadConfigFromReader(strings.NewReader(strings.ReplaceAll(capacityModels, "evict_cost: 1}", "evict_cost: 9223372036854775807}") + capacityRouting))
	require.ErrorContains(t, err, "sum of capacity.evict_cost")
}

func TestConfig_CapacityConfigSources(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "models.yaml", capacityModels)
	writeYAML(t, dir, "routing.yaml", capacityRouting)
	cfg, err := LoadConfigSources("", dir)
	require.NoError(t, err)
	require.Equal(t, "capacity", cfg.Routing.Router.Use)
	writeYAML(t, dir, "duplicate.yaml", capacityRouting)
	_, err = LoadConfigSources("", dir)
	require.ErrorContains(t, err, `duplicate capacity "total"`)
}

func TestConfig_CapacitySpillover(t *testing.T) {
	for _, total := range []int{9, 10} {
		t.Run(fmt.Sprint(total), func(t *testing.T) {
			input := capacityModels + strings.Replace(capacityRouting, "total: 10", fmt.Sprintf("total: %d", total), 1) + `selectors:
  spill:
    strategy: spillover
    targets: [a, b]
`
			_, err := LoadConfigFromReader(strings.NewReader(input))
			if total == 10 {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "spillover targets exceed capacity")
			}
		})
	}
}

func TestConfig_CapacitySchema(t *testing.T) {
	data, err := os.ReadFile("../../config-schema.json")
	require.NoError(t, err)
	var schema jsonschema.Schema
	require.NoError(t, json.Unmarshal(data, &schema))
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{BaseURI: "https://github.com/mostlygeek/llama-swap/"})
	require.NoError(t, err)
	// These configurations have valid field shapes but invalid relationships.
	// ValidateCapacity and routing validation remain the authority for them.
	loaderRejects := map[string]bool{
		"missing capacity settings":        true,
		"inactive settings":                true,
		"matrix settings":                  true,
		"group settings":                   true,
		"legacy groups":                    true,
		"legacy matrix":                    true,
		"scalar model shape in named mode": true,
	}
	for _, tc := range []struct {
		name, input string
		valid       bool
	}{
		{"pools", poolsConfig, true},
		{"pools missing reference", strings.Replace(poolsConfig, "pool: gpu\n          usage: 6", "usage: 6", 1), false},
		{"pools fractional usage", strings.Replace(poolsConfig, "usage: 6", "usage: 6.5", 1), false},
		{"pools mixed router", strings.Replace(poolsConfig, "        pools:", "        total: 100\n        pools:", 1), false},
		{"pools mixed model", strings.Replace(poolsConfig, "      evict_cost: 10", "      usage: 1\n      evict_cost: 10", 1), false},
		// Router/model relationships are enforced by the loader, not the schema.
		{"scalar model shape in named mode", strings.Replace(poolsConfig, "      pools:\n        - pool: ram\n          usage: 30", "      usage: 30", 1), true},
		{"exclusive null", "models: {a: {cmd: echo a, proxy: http://localhost:8080, capacity: null}}\n" + capacityRouting, true},
		{"exclusive empty", "models: {a: {cmd: echo a, proxy: http://localhost:8080, capacity: {}}}\n" + capacityRouting, true},
		{"exclusive cost", "models: {a: {cmd: echo a, proxy: http://localhost:8080, capacity: {evict_cost: 7}}}\n" + capacityRouting, true},

		{"valid", capacityModels + capacityRouting, true},
		{"missing total", capacityModels + strings.Replace(capacityRouting, "total: 10", "{}", 1), false},
		{"zero total", capacityModels + strings.Replace(capacityRouting, "total: 10", "total: 0", 1), false},
		{"fraction usage", strings.Replace(capacityModels, "usage: 4", "usage: 4.5", 1) + capacityRouting, false},
		{"negative cost", strings.Replace(capacityModels, "evict_cost: 1}", "evict_cost: -1}", 1) + capacityRouting, false},
		{"zero cost", strings.Replace(capacityModels, "evict_cost: 1}", "evict_cost: 0}", 1) + capacityRouting, false},
		{"missing usage", strings.Replace(capacityModels, "usage: 4, ", "", 1) + capacityRouting, true},
		{"missing model capacity", strings.Replace(capacityModels, "    capacity: {usage: 4, evict_cost: 1}\n", "", 1) + capacityRouting, true},
		{"default cost", strings.Replace(capacityModels, ", evict_cost: 1", "", 1) + capacityRouting, true},
		{"missing capacity settings", capacityModels + "routing: {router: {use: capacity}}", true},
		{"inactive settings", capacityModels + strings.Replace(capacityRouting, "use: capacity", "use: matrix", 1), true},
		{"matrix settings", capacityModels + strings.Replace(capacityRouting, "    settings:\n", "    settings:\n      matrix: {vars: {a: a}, sets: {s: a}}\n", 1), true},
		{"group settings", capacityModels + strings.Replace(capacityRouting, "    settings:\n", "    settings:\n      groups: {}\n", 1), true},
		{"legacy groups", capacityModels + capacityRouting + "groups: {}", true},
		{"legacy matrix", capacityModels + capacityRouting + "matrix: {vars: {a: a}, sets: {s: a}}", true},
		{"default group", capacityModels, true},
		{"legacy group", capacityModels + "groups: {g: {members: [a,b]}}", true},
		{"legacy matrix compatible", capacityModels + "matrix: {vars: {a: a}, sets: {s: a}}", true},
		{"inactive matrix compatible", capacityModels + "routing: {router: {use: group, settings: {groups: {}, matrix: {vars: {a: a}, sets: {s: a}}}}}", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var instance any
			require.NoError(t, yaml.Unmarshal([]byte(tc.input), &instance))
			data, err := json.Marshal(instance)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &instance))
			err = resolved.Validate(instance)
			if loaderRejects[tc.name] {
				_, loadErr := LoadConfigFromReader(strings.NewReader(tc.input))
				require.Error(t, loadErr, "the loader must still reject invalid relationships")
			}

			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
