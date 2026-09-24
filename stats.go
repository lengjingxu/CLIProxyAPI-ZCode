package main

import (
	"sort"
	"sync"
	"time"
)

// counters keeps the in-process view of how much of the ZCode quota this proxy
// has spent. Token numbers come from the upstream usage blocks.
type counters struct {
	mu           sync.Mutex
	requests     int64
	failures     int64
	inputTokens  int64
	outputTokens int64
	lastError    string
	lastErrorAt  time.Time
	lastLatency  time.Duration
	perModel     map[string]*modelCounters
}

type modelCounters struct {
	Requests     int64
	Failures     int64
	InputTokens  int64
	OutputTokens int64
}

type modelCountersSnapshot struct {
	Model        string
	Requests     int64
	Failures     int64
	InputTokens  int64
	OutputTokens int64
}

type countersSnapshot struct {
	Requests     int64
	Failures     int64
	InputTokens  int64
	OutputTokens int64
	LastError    string
	LastErrorAt  time.Time
	LastLatency  time.Duration
	PerModel     []modelCountersSnapshot
}

var stats = &counters{perModel: make(map[string]*modelCounters)}

func (c *counters) record(model string, inputTokens, outputTokens int64, failure string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	c.inputTokens += inputTokens
	c.outputTokens += outputTokens
	entry := c.modelEntry(model)
	entry.Requests++
	entry.InputTokens += inputTokens
	entry.OutputTokens += outputTokens
	if failure != "" {
		c.failures++
		entry.Failures++
		c.lastError = truncateRunes(failure, 300)
		c.lastErrorAt = time.Now()
	}
}

func (c *counters) recordFailure(model, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures++
	c.modelEntry(model).Failures++
	c.lastError = truncateRunes(message, 300)
	c.lastErrorAt = time.Now()
}

func (c *counters) observeLatency(duration time.Duration) {
	c.mu.Lock()
	c.lastLatency = duration
	c.mu.Unlock()
}

func (c *counters) modelEntry(model string) *modelCounters {
	if model == "" {
		model = "unknown"
	}
	entry, ok := c.perModel[model]
	if !ok {
		entry = &modelCounters{}
		c.perModel[model] = entry
	}
	return entry
}

func (c *counters) snapshot() countersSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := countersSnapshot{
		Requests:     c.requests,
		Failures:     c.failures,
		InputTokens:  c.inputTokens,
		OutputTokens: c.outputTokens,
		LastError:    c.lastError,
		LastErrorAt:  c.lastErrorAt,
		LastLatency:  c.lastLatency,
		PerModel:     make([]modelCountersSnapshot, 0, len(c.perModel)),
	}
	for model, entry := range c.perModel {
		out.PerModel = append(out.PerModel, modelCountersSnapshot{
			Model:        model,
			Requests:     entry.Requests,
			Failures:     entry.Failures,
			InputTokens:  entry.InputTokens,
			OutputTokens: entry.OutputTokens,
		})
	}
	sort.Slice(out.PerModel, func(i, j int) bool {
		if out.PerModel[i].Requests == out.PerModel[j].Requests {
			return out.PerModel[i].Model < out.PerModel[j].Model
		}
		return out.PerModel[i].Requests > out.PerModel[j].Requests
	})
	return out
}
