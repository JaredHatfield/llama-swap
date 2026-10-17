---
title: Routing capacity and request queues
summary: Configure model residency budgets with capacity pools, inference concurrency limits, and request queues.
category: guides
tags: [routing, queue, capacity, concurrency, concurrency-limit, max-concurrent-requests, global-concurrency-limit, rate-limit]
config_keys: [routing, routing.router.settings.capacity, models.*.capacity, models.*.concurrencyLimit, globalConcurrencyLimit]
updated: 2026-10-17
---

# Routing capacity and request queues

There is no `models.*.maxConcurrentRequests` setting. The real per-model key
is `models.*.concurrencyLimit`, which limits active parallel requests.

The router serializes model swaps and queues requests until the selected model
is ready. Avoid using many simultaneous client retries as a capacity control:
they create more queued work. Choose a routing policy that fits the models that
may coexist, then set client timeouts high enough for the queue and load time.

Inspect the Activity view and logs when latency grows. A queue that never drains
usually means the command, proxy, or health check is wrong; see
`guides/model-runtime/troubleshooting-model-wont-load`.

## Global concurrency limit

`globalConcurrencyLimit` is a top-level setting, separate from
`models.*.concurrencyLimit`. It caps the number of inference requests served
at once across every model combined, using a single shared semaphore:

```yaml
globalConcurrencyLimit: 8
```

The default is `0`, meaning no limit — the semaphore is not even added to the
request chain, so a default config pays no cost for the feature. Once the
limit is reached, further requests are rejected immediately with an HTTP 429
response rather than queued; there is no wait for a slot to free up. Clients
should treat a 429 as a signal to back off and retry, the same way they handle
a per-model concurrency limit rejection.

Use this to protect shared hardware (CPU, disk, network) from being
overwhelmed by traffic spread across many different models, which a per-model
`concurrencyLimit` cannot do since it only counts requests to one model at a
time.

## Model residency budgets

Select `capacity` to keep models resident while their declared usage fits every
pool. Pool units are operator-defined; placement still belongs in the command.
For example, with the test responder built by `make simple-responder` on macOS:

```yaml
routing:
  router:
    use: capacity
    settings:
      capacity:
        pools:
          - name: ram
            total: 100
          - name: gpu0
            total: 24
models:
  chat:
    cmd: ./build/simple-responder_darwin_arm64 -port ${PORT}
    capacity:
      evict_cost: 50
      pools:
        - pool: ram
          usage: 40
        - pool: gpu0
          usage: 20
  embeddings:
    cmd: ./build/simple-responder_darwin_arm64 -port ${PORT}
    capacity:
      pools:
        - pool: ram
          usage: 10
        - pool: gpu0
          usage: 3
```

These models fit together. Unlisted pools consume zero. To add a model spanning
several GPUs, list each pool and its usage. Each eviction releases all of that
model's pools; its `evict_cost` is counted once (default `1`, higher protects it).
The planner favors lower summed eviction cost, then fewer evictions, then sorted
model IDs. Search is bounded to 100,000 nodes; on exhaustion it uses the best
feasible plan found, which may not be globally cheapest. Swap logs report
`search_limited=true` in that case. Pending starts reserve resources, and busy
victims finish their requests before unloading.

Existing single-budget configs still use `capacity: {total: 100}` on the router
and `capacity: {usage: 40, evict_cost: 50}` on a model. Choose either this form or
named pools; do not mix them. Existing group and matrix configurations retain
their behavior. Capacity routing cannot be combined with group/matrix settings.

A missing, null, empty, or cost-only model capacity block makes that model
exclusive: loading it unloads all other local models, and loading another model
unloads it. Explicit empty pool lists, unknown/duplicate names, nonpositive or
fractional numbers, and usage exceeding its pool total fail validation. Spillover
selector targets must fit together in every pool and cannot include an exclusive
model alongside another local target. Remote peer models are outside this local
budget. Estimates that understate real usage can still cause an out-of-memory
failure; leave headroom for runtime overhead.
