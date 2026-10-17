---
title: Running several models at once with groups, matrix and capacity
summary: Choosing between group, matrix and capacity routers, and how each decides what gets unloaded.
category: guides
tags: [routing, groups, matrix, capacity, concurrency, swap, vram, memory]
config_keys: [routing, routing.router.use, routing.router.settings.groups, routing.router.settings.matrix, routing.router.settings.capacity, models.*.capacity]
updated: 2026-10-17
---

# Running several models at once: groups, matrix and capacity

Out of the box llama-swap runs one model at a time. The `routing` section
changes that. There are three engines and you pick one:

```yaml
routing:
  router:
    use: group     # or: matrix, capacity
```

## `group` — the default, simpler

You define named groups of models and set two flags per group.

```yaml
routing:
  router:
    use: group
    settings:
      groups:
        # one member at a time; persistent groups can stay loaded
        main:
          swap: true         # only one member runs at a time
          exclusive: true    # unloads other non-persistent groups
          members: [llama, qwen]

        # these three can coexist; an exclusive group can evict them
        small:
          swap: false        # all members can be loaded at once
          exclusive: false   # loading one doesn't unload other groups
          members: [embeddings, reranker, whisper]

        # never unloaded by anything else
        forever:
          persistent: true
          swap: false
          exclusive: false
          members: [always-on-embeddings]
```

The three flags:

- **`swap`** (default `true`) — how members behave among *themselves*. `true`
  means one at a time; `false` means they all coexist.
- **`exclusive`** (default `true`) — how the group affects *other* groups.
  `true` means loading a member unloads other non-persistent groups.
- **`persistent`** (default `false`) — other groups can never unload this one.

**A model can belong to only one group**, and every member must be a real model
ID.

The classic setup is one exclusive group for the big LLMs and one non-exclusive
group for small always-useful models like embeddings and rerankers.

## `matrix` — more work, far more flexible

The matrix router takes a list of model combinations that are allowed to run
concurrently and solves for the cheapest way to satisfy each request.

```yaml
routing:
  router:
    use: matrix
    settings:
      matrix:
        vars:
          g: gemma-model
          q: qwen-model
          v: voxtral-model

        evict_costs:
          v: 50              # vLLM, slow cold start — avoid evicting
          llama-70B: 30

        sets:
          standard:    "(g | q) & v"
          creative:    "(g | q) & stable-diffusion"
          full:        "llama-70B"
```

`sets` values are expressions:

| operator | meaning |
| --- | --- |
| `&` | AND — these run together |
| `\|` | OR — alternatives |
| `()` | grouping |
| `+name` | inline another set's expression |

`"(g | q) & v"` expands to `[gemma-model, voxtral-model]` and
`[qwen-model, voxtral-model]`.
Parenthesize mixed expressions so their intended capacity rule is obvious, and
test every requested combination: an expression that excludes a needed set can
make the router evict a model unexpectedly.

How the solver works when a request for model X arrives:

1. If X is already running, forward the request.
2. Otherwise collect every set containing X.
3. For each set, sum the `evict_costs` of running models *not* in that set.
4. Pick the lowest-cost set, ties broken by definition order.
5. Evict the models outside it, start X, forward the request.

Two things worth internalising:

- **Subsets are permitted.** A set `[a, b, c]` also allows `[a, b]`, `[a]` and
  so on. Only the requested model is started; the rest are not preloaded.
- **A model in no set can only run alone.**

`evict_costs` (default 1) is how you express "this one is painful to reload".
Give slow cold-starting backends a high cost.

When you deploy the head end with the kubeswap Helm chart, the chart's
`config.matrix` values can *generate* this whole section from the model
roster instead of you writing the DSL (see the kubeswap-kubernetes
article's matrix-builder section); hand-written routing and the builder are
mutually exclusive.

### Use `+auto:orphans` for the models that no set names

A leaf is a model ID or a var in a set expression. The reserved reference
`+auto:orphans` includes each model that is not a leaf in your sets. llama-swap
changes a var to its model ID before this test. A reference such as `+base` is
not a leaf and adds no models.

The name of the set is `auto:orphans`. The first `+` is the usual syntax for a
reference to a set. Only a reference name can contain a colon. A model name or a
var name cannot contain a colon.

llama-swap makes the orphan list one time, when it reads the configuration, and
sorts the list by model ID. The expression is therefore always the same. For
example, a leaf names `a`, but no leaf names `b` or `c`. Then `+auto:orphans`
operates as `(b | c)`:

```yaml
sets:
  always:  "task-small & embed-model"
  main:    "+always & moe-medium & (dense-a | dense-b)"
  scratch: "+always & +auto:orphans"
```

If there are no orphan models, llama-swap removes the `+auto:orphans` term. An
empty term also moves through references. For example, the set `empty` resolves
only to an empty `+auto:orphans`. The set `outer: "+empty & x"` then operates as
`x`. llama-swap can never select a set that contains only an empty
`+auto:orphans`.

If you define a set with the name `auto:orphans`, llama-swap uses your set and
does not make the automatic set.

If a set refers to `+auto:orphans`, llama-swap writes one of these lines to the
log at startup and at each reload of the configuration:

- `matrix: synthesized set "auto:orphans" = (modelA | modelB | ...)`
- `matrix: synthesized set "auto:orphans" is empty; +auto:orphans terms dropped`
- `matrix: set "auto:orphans" is user-defined; orphan synthesis disabled`

If you change `models:`, llama-swap makes a new orphan list at the next reload.
This is the intended behavior.

## `capacity` — declared resource pools

Declare independent pools and each model's usage in them. Models can coexist
when their summed usage fits **every** pool. Units are operator-defined and may
be different for each pool. The names below describe RAM and GPU memory in GiB;
llama-swap neither measures usage nor selects devices. Configure device placement
in the model command or environment, and leave headroom for runtime overhead.
The usage values are examples; measure them for your own models and settings.

```yaml
routing:
  router:
    use: capacity
    settings:
      capacity:
        pools:
          - name: ram
            total: 128
          - name: gpu0
            total: 24
          - name: gpu1
            total: 48
models:
  chat:
    cmd: llama-server -m /models/chat.gguf --port ${PORT}
    env:
      - CUDA_VISIBLE_DEVICES=0
    capacity:
      evict_cost: 50
      pools:
        - pool: ram
          usage: 8
        - pool: gpu0
          usage: 20
  embeddings:
    cmd: llama-server -m /models/embeddings.gguf --embedding --port ${PORT}
    env:
      - CUDA_VISIBLE_DEVICES=0
    capacity:
      evict_cost: 10
      pools:
        - pool: ram
          usage: 4
        - pool: gpu0
          usage: 3
  large:
    cmd: llama-server -m /models/large.gguf --port ${PORT}
    env:
      - CUDA_VISIBLE_DEVICES=1
    capacity:
      evict_cost: 100
      pools:
        - pool: ram
          usage: 32
        - pool: gpu1
          usage: 40
```

All three models fit together: RAM usage is 44, GPU0 usage is 23, and GPU1 usage
is 40. A model can also span GPUs by listing usage for both GPU pools and using
a command configured for both devices. Unlisted pools consume zero.

`evict_cost` is a positive integer on the **model**, default `1`. Stopping a model
releases its usage from every pool and counts its eviction cost once. When a new
model does not fit, the planner favors the lowest summed eviction cost, then the
fewest victims, then lexicographically sorted victim IDs. For example, if freeing
10 units can be achieved by evicting one model with cost 50 or two models with
cost 1 each, it favors the two cheaper evictions.

Search considers resident models and pending starts, with a budget of 100,000
search nodes. It starts with a feasible eviction plan and improves it; if the
budget runs out, the result still fits every pool but may not be globally
cheapest. Swap logs report `search_limited=true` when this happens. Victims finish
active requests before unloading; only the requested model starts.

### Single-budget compatibility

Existing scalar configs remain supported. Use `total` instead of router `pools`,
and `usage` instead of model `pools`:

```yaml
routing:
  router:
    use: capacity
    settings:
      capacity:
        total: 100
models:
  chat:
    cmd: llama-server -m /models/chat.gguf --port ${PORT}
    capacity:
      usage: 60
      evict_cost: 50
```

Choose one form for the configuration; do not mix scalar usage with named pools.

### Defaults and validation

Missing, null, empty, or cost-only model capacity blocks make the model
**exclusive in both directions**: loading it unloads all other local models,
and loading another model unloads it. Missing declarations therefore allow an
existing model to run conservatively without estimating its usage first.

Explicit declarations must use positive integers. Empty pool lists, unknown or
duplicate pool names, usage exceeding a pool's total, and overflowing sums fail
configuration validation. The JSON schema checks field shapes and local
constraints; the Go loader checks relationships between router pools and models.
Remove group/matrix settings, including legacy top-level `groups`/`matrix`, when
selecting `capacity`. Peer models do not consume local capacity. Local spillover
targets must fit together in every pool and cannot coexist with an exclusive
model. TTL and explicit unload release usage when the process stops.

Accounting is per router instance. During reload, the new server is constructed
before the old one shuts down; requests or preloads can briefly overlap old
processes. Allow headroom when reloading near physical limits.

## Which one?

Use **group** if your setup is describable as "these run together, those swap
out". It is easier to read and easier to get right.

Use **matrix** when the combinations depend on which models are involved — a
70B that needs every GPU alone, versus several small models that fit together,
versus a mid-size LLM plus TTS. Groups cannot express that; matrix can.

Use **capacity** when combinations are determined by additive resource budgets,
including several GPUs or shared RAM, and listing matrix combinations would be
tedious.

## Request ordering

Queued requests are FIFO. You can give some models priority:

```yaml
routing:
  scheduler:
    use: fifo
    settings:
      fifo:
        priority:
          interactive-model: 10
          batch-model: 1
```

Higher numbers are serviced first. Models default to 0.

## Related

- `reference/config/routing` — the full annotated section
- `guides/model-runtime/ttl-and-unloading` — reclaiming VRAM from idle models
- `guides/operations/kubeswap-kubernetes` — the kubeswap Helm chart, whose
  `config.matrix` builds this section from the model roster
