package config

import (
	"fmt"
	"math"
	"strings"

	"gopkg.in/yaml.v3"
)

// CapacityConfig declares independent named resource pools.
// Units are operator-chosen and may differ between pools.
type CapacityConfig struct {
	Pools []CapacityPoolConfig `yaml:"pools"`
}

type CapacityPoolConfig struct {
	Name  string `yaml:"name"`
	Total int64  `yaml:"total"`
}

type ModelCapacityPoolConfig struct {
	Pool  string `yaml:"pool"`
	Usage int64  `yaml:"usage"`
}

type ModelCapacityConfig struct {
	EvictCost int64                     `yaml:"evict_cost"`
	Pools     []ModelCapacityPoolConfig `yaml:"pools,omitempty"`
}

func (c *CapacityConfig) UnmarshalYAML(node *yaml.Node) error {
	type rawCapacity CapacityConfig
	var raw rawCapacity
	if err := decodeCapacity(node, &raw, "pools"); err != nil {
		return err
	}
	*c = CapacityConfig(raw)
	return nil
}

func (c *ModelCapacityConfig) UnmarshalYAML(node *yaml.Node) error {
	type rawCapacity ModelCapacityConfig
	raw := rawCapacity{EvictCost: 1}
	if err := decodeCapacity(node, &raw, "evict_cost", "pools"); err != nil {
		return err
	}
	*c = ModelCapacityConfig(raw)
	return nil
}

func (c *CapacityPoolConfig) UnmarshalYAML(node *yaml.Node) error {
	type raw CapacityPoolConfig
	return decodeCapacity(node, (*raw)(c), "name", "total")
}

func (c *ModelCapacityPoolConfig) UnmarshalYAML(node *yaml.Node) error {
	type raw ModelCapacityPoolConfig
	return decodeCapacity(node, (*raw)(c), "pool", "usage")
}

// Reject numeric truncation, unknown fields, null values and malformed lists.
func decodeCapacity(node *yaml.Node, dst any, fields ...string) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("capacity must be a mapping")
	}
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		known := false
		for _, field := range fields {
			if key == field {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("unknown capacity setting %q", key)
		}
		switch key {
		case "pools":
			if value.Kind != yaml.SequenceNode || len(value.Content) == 0 {
				return fmt.Errorf("capacity.pools must be a nonempty list")
			}
		case "name", "pool":
			if value.Tag != "!!str" || strings.TrimSpace(value.Value) == "" {
				return fmt.Errorf("capacity.%s must be a nonempty string", key)
			}
		default:
			if value.Tag != "!!int" {
				return fmt.Errorf("capacity.%s must be an integer", key)
			}
		}
	}
	return node.Decode(dst)
}

// Exclusive models have no usage declaration and cannot coexist with any model.
func (c *ModelCapacityConfig) Exclusive() bool {
	return c == nil || c.Pools == nil
}

func (c *ModelCapacityConfig) EvictionCost() int64 {
	if c == nil {
		return 1
	}
	return c.EvictCost
}

// Totals and UsageVector use the declared pool order.
func (c *CapacityConfig) Totals() []int64 {
	totals := make([]int64, len(c.Pools))
	for i, pool := range c.Pools {
		totals[i] = pool.Total
	}
	return totals
}

func (c *CapacityConfig) UsageVector(model *ModelCapacityConfig) []int64 {
	usage := make([]int64, len(c.Pools))
	if model.Exclusive() {
		return usage
	}
	for _, requirement := range model.Pools {
		for i, pool := range c.Pools {
			if pool.Name == requirement.Pool {
				usage[i] = requirement.Usage
				break
			}
		}
	}
	return usage
}

func (c ModelCapacityConfig) Validate() error {
	if c.EvictCost <= 0 {
		return fmt.Errorf("capacity.evict_cost must be a positive integer")
	}
	return nil
}

// ValidateCapacity rejects ambiguous declarations and bounds planner arithmetic.
func ValidateCapacity(c *CapacityConfig, models map[string]ModelConfig) error {
	if c == nil {
		return fmt.Errorf("capacity configuration is required")
	}
	if len(c.Pools) == 0 {
		return fmt.Errorf("pools must contain at least one named pool")
	}
	names := make(map[string]struct{}, len(c.Pools))
	for _, pool := range c.Pools {
		if strings.TrimSpace(pool.Name) == "" {
			return fmt.Errorf("pool name must be a nonempty string")
		}
		if _, ok := names[pool.Name]; ok {
			return fmt.Errorf("pools contains duplicate pool name %q", pool.Name)
		}
		if pool.Total <= 0 {
			return fmt.Errorf("pool %q total must be a positive integer", pool.Name)
		}
		names[pool.Name] = struct{}{}
	}
	totals := c.Totals()
	sums := make([]int64, len(totals))
	var cost int64
	for id, model := range models {
		mc := model.Capacity
		if mc != nil {
			if err := mc.Validate(); err != nil {
				return fmt.Errorf("model %q: %w", id, err)
			}
			if mc.Pools != nil && len(mc.Pools) == 0 {
				return fmt.Errorf("model %q: pools must be a nonempty list", id)
			}
			seen := make(map[string]struct{}, len(mc.Pools))
			for _, pool := range mc.Pools {
				if _, ok := names[pool.Pool]; !ok {
					return fmt.Errorf("model %q: unknown pool %q", id, pool.Pool)
				}
				if _, duplicate := seen[pool.Pool]; duplicate {
					return fmt.Errorf("model %q: capacity.pools contains duplicate pool %q", id, pool.Pool)
				}
				seen[pool.Pool] = struct{}{}
				if pool.Usage <= 0 {
					return fmt.Errorf("model %q: pool %q usage must be a positive integer", id, pool.Pool)
				}
			}
		}
		for i, amount := range c.UsageVector(mc) {
			if amount > totals[i] {
				return fmt.Errorf("model %q: usage (%d) exceeds total (%d) for pool %q", id, amount, totals[i], c.Pools[i].Name)
			}
			if amount > math.MaxInt64-sums[i] {
				return fmt.Errorf("sum of capacity pool usage exceeds int64 range for pool %q", c.Pools[i].Name)
			}
			sums[i] += amount
		}
		if mc.EvictionCost() > math.MaxInt64-cost {
			return fmt.Errorf("sum of capacity.evict_cost exceeds int64 range")
		}
		cost += mc.EvictionCost()
	}
	return nil
}
