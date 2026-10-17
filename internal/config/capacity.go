package config

import (
	"fmt"
	"math"
	"strings"

	"gopkg.in/yaml.v3"
)

// CapacityConfig declares either one abstract budget or independent named pools.
// Units are operator-chosen and may differ between pools.
type CapacityConfig struct {
	Total    int64                `yaml:"total,omitempty"`
	Pools    []CapacityPoolConfig `yaml:"pools,omitempty"`
	totalSet bool
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
	Usage     int64                     `yaml:"usage,omitempty"`
	EvictCost int64                     `yaml:"evict_cost"`
	Pools     []ModelCapacityPoolConfig `yaml:"pools,omitempty"`
	usageSet  bool
}

func (c *CapacityConfig) UnmarshalYAML(node *yaml.Node) error {
	type rawCapacity CapacityConfig
	var raw rawCapacity
	if err := decodeCapacity(node, &raw, "total", "pools"); err != nil {
		return err
	}
	*c = CapacityConfig(raw)
	c.totalSet = capacityHasKey(node, "total")
	return nil
}

func (c *ModelCapacityConfig) UnmarshalYAML(node *yaml.Node) error {
	type rawCapacity ModelCapacityConfig
	raw := rawCapacity{EvictCost: 1}
	if err := decodeCapacity(node, &raw, "usage", "evict_cost", "pools"); err != nil {
		return err
	}
	*c = ModelCapacityConfig(raw)
	c.usageSet = capacityHasKey(node, "usage")
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

func capacityHasKey(node *yaml.Node, key string) bool {
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return true
		}
	}
	return false
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
	return c == nil || (!c.usageSet && c.Usage == 0 && c.Pools == nil)
}

func (c *ModelCapacityConfig) EvictionCost() int64 {
	if c == nil {
		return 1
	}
	return c.EvictCost
}

// Totals and UsageVector normalize both config forms into the same pool order.
func (c *CapacityConfig) Totals() []int64 {
	if c.Pools == nil {
		return []int64{c.Total}
	}
	totals := make([]int64, len(c.Pools))
	for i, pool := range c.Pools {
		totals[i] = pool.Total
	}
	return totals
}

func (c *CapacityConfig) UsageVector(model *ModelCapacityConfig) []int64 {
	usage := make([]int64, len(c.Totals()))
	if model.Exclusive() {
		return usage
	}
	if c.Pools == nil {
		usage[0] = model.Usage
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
	if (c.usageSet || c.Usage != 0) && c.Usage <= 0 {
		return fmt.Errorf("capacity.usage must be a positive integer")
	}
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
	if c.Pools != nil && (c.totalSet || c.Total != 0) {
		return fmt.Errorf("total and pools cannot be combined")
	}
	if c.Pools == nil && c.Total <= 0 {
		return fmt.Errorf("total must be a positive integer")
	}
	if c.Pools != nil && len(c.Pools) == 0 {
		return fmt.Errorf("pools must be a nonempty list")
	}
	names := make(map[string]int)
	for i, pool := range c.Pools {
		if strings.TrimSpace(pool.Name) == "" {
			return fmt.Errorf("pool name must be a nonempty string")
		}
		if _, ok := names[pool.Name]; ok {
			return fmt.Errorf("duplicate pool %q", pool.Name)
		}
		if pool.Total <= 0 {
			return fmt.Errorf("pool %q total must be a positive integer", pool.Name)
		}
		names[pool.Name] = i
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
			scalar := mc.usageSet || mc.Usage != 0
			if scalar && mc.Pools != nil {
				return fmt.Errorf("model %q: usage and pools cannot be combined", id)
			}
			if c.Pools != nil && scalar {
				return fmt.Errorf("model %q: named pools require capacity.pools", id)
			}
			if c.Pools == nil && mc.Pools != nil {
				return fmt.Errorf("model %q: capacity.pools requires named router pools", id)
			}
			if mc.Pools != nil && len(mc.Pools) == 0 {
				return fmt.Errorf("model %q: pools must be a nonempty list", id)
			}
			seen := make(map[string]bool)
			for _, pool := range mc.Pools {
				if _, ok := names[pool.Pool]; !ok {
					return fmt.Errorf("model %q: unknown pool %q", id, pool.Pool)
				}
				if seen[pool.Pool] {
					return fmt.Errorf("model %q: duplicate pool %q", id, pool.Pool)
				}
				seen[pool.Pool] = true
				if pool.Usage <= 0 {
					return fmt.Errorf("model %q: pool %q usage must be a positive integer", id, pool.Pool)
				}
			}
		}
		for i, amount := range c.UsageVector(mc) {
			if amount > totals[i] {
				return fmt.Errorf("model %q: capacity.usage (%d) exceeds total (%d) in pool %d", id, amount, totals[i], i)
			}
			if amount > math.MaxInt64-sums[i] {
				return fmt.Errorf("sum of capacity.usage exceeds int64 range in pool %d", i)
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
