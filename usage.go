package main

// usage.go implements the UsagePlugin capability (see sdk/pluginabi
// MethodUsageHandle). After every request the host pushes the canonical
// pluginapi.UsageRecord here — it already persists the token counters into its
// own usage manager for the management UI, so this plugin only emits one
// compact forwarding-log line (model, latency, tokens, failure) to stderr,
// which the host captures into its log stream. The record shape mirrors the
// host's wire format: Go field names, nested Detail and Failure objects.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// usageLogLine is the stable marker that lets users grep forwarding logs out
// of the host log stream.
const usageLogLine = "[oc2cpa-usage]"

// usageStoreCapacity bounds the in-memory record window backing the usage
// panel: the newest entries are kept, older ones are dropped. Records live
// only for the host process lifetime; durable statistics stay in the host's
// own usage database / management UI.
const usageStoreCapacity = 1000

// usageEntry is one stored request for the usage panel. Lowercase json tags
// are the panel's API contract.
type usageEntry struct {
	Time         string `json:"time"`
	RequestID    string `json:"request_id,omitempty"`
	AuthID       string `json:"auth_id,omitempty"`
	AuthType     string `json:"auth_type,omitempty"`
	APIKey       string `json:"api_key,omitempty"`
	Model        string `json:"model"`
	Alias        string `json:"alias,omitempty"`
	Stream       bool   `json:"stream"`
	Failed       bool   `json:"failed"`
	Status       int    `json:"status,omitempty"`
	LatencyMS    int64  `json:"latency_ms"`
	TTFTMS       int64  `json:"ttft_ms,omitempty"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Reasoning    int64  `json:"reasoning_tokens,omitempty"`
	Cached       int64  `json:"cached_tokens,omitempty"`
	TotalTokens  int64  `json:"total_tokens"`
}

var usageStoreMu sync.Mutex
var usageStore []usageEntry

// usageLogEvent is the JSON shape of one forwarding-log line. Lowercase keys
// keep the console output human-friendly; counters are always present so
// log scrapers can rely on the schema.
type usageLogEvent struct {
	Time          string `json:"time"`
	RequestID     string `json:"request_id,omitempty"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	ResponseModel string `json:"response_model,omitempty"`
	AuthType      string `json:"auth_type,omitempty"`
	APIKey        string `json:"api_key,omitempty"`
	Stream        bool   `json:"stream"`
	Failed        bool   `json:"failed"`
	Status        int    `json:"status,omitempty"`
	LatencyMS     int64  `json:"latency_ms"`
	TTFTMS        int64  `json:"ttft_ms,omitempty"`
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	Reasoning     int64  `json:"reasoning_tokens,omitempty"`
	Cached        int64  `json:"cached_tokens,omitempty"`
	TotalTokens   int64  `json:"total_tokens"`
}

// handleUsage renders and prints the forwarding-log line for one record.
// Logging is strictly best-effort: a malformed record or a disabled switch
// simply no-ops, because the host logs a plugin fault if this RPC errors and
// usage telemetry must never break request handling.
func handleUsage(requestJSON []byte) error {
	var record pluginapi.UsageRecord
	if err := json.Unmarshal(requestJSON, &record); err != nil {
		return nil
	}
	// Only log this plugin's own records; the host routes other plugins' usage
	// to their owners, but defensive filtering keeps the log honest if a host
	// ever broadcasts globally.
	if record.Provider != "" && record.Provider != providerName {
		return nil
	}
	if !readSettings().UsageLog {
		return nil
	}
	entry := usageEntryFromRecord(record)
	usageStoreMu.Lock()
	usageStore = append(usageStore, entry)
	if len(usageStore) > usageStoreCapacity {
		usageStore = usageStore[len(usageStore)-usageStoreCapacity:]
	}
	usageStoreMu.Unlock()
	fmt.Fprintln(os.Stderr, usageLogLine+" "+formatUsageLog(record))
	return nil
}

// usageEntryFromRecord maps the host's canonical UsageRecord into the compact
// panel entry. client-sensitive identifiers are reduced the same way as the
// console log line.
func usageEntryFromRecord(record pluginapi.UsageRecord) usageEntry {
	entry := usageEntry{
		Time:         record.RequestedAt.Format(time.RFC3339Nano),
		RequestID:    record.RequestID,
		AuthID:       record.AuthID,
		AuthType:     record.AuthType,
		APIKey:       redactKey(record.APIKey),
		Model:        record.Model,
		Alias:        record.Alias,
		Stream:       record.Stream,
		Failed:       record.Failed,
		LatencyMS:    record.Latency.Milliseconds(),
		InputTokens:  record.Detail.InputTokens,
		OutputTokens: record.Detail.OutputTokens,
		Reasoning:    record.Detail.ReasoningTokens,
		Cached:       record.Detail.CachedTokens + record.Detail.CacheReadTokens,
		TotalTokens:  record.Detail.TotalTokens,
	}
	if record.TTFT > 0 {
		entry.TTFTMS = record.TTFT.Milliseconds()
	}
	if record.Failed {
		entry.Status = record.Failure.StatusCode
	}
	if entry.Time == "" || entry.Time == "0001-01-01T00:00:00Z" {
		entry.Time = time.Now().Format(time.RFC3339Nano)
	}
	return entry
}

// snapshotUsageEntries returns a copy of the newest entries (oldest first).
func snapshotUsageEntries() []usageEntry {
	usageStoreMu.Lock()
	defer usageStoreMu.Unlock()
	out := make([]usageEntry, len(usageStore))
	copy(out, usageStore)
	return out
}

// usageBucket accumulates token counters and request outcomes.
type usageBucket struct {
	Requests     int64 `json:"requests"`
	Success      int64 `json:"success"`
	Failed       int64 `json:"failed"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	Reasoning    int64 `json:"reasoning_tokens,omitempty"`
	Cached       int64 `json:"cached_tokens,omitempty"`
	TotalTokens  int64 `json:"total_tokens"`
}

func (b *usageBucket) add(entry usageEntry) {
	b.Requests++
	if entry.Failed {
		b.Failed++
	} else {
		b.Success++
	}
	b.InputTokens += entry.InputTokens
	b.OutputTokens += entry.OutputTokens
	b.Reasoning += entry.Reasoning
	b.Cached += entry.Cached
	b.TotalTokens += entry.TotalTokens
}

// modelUsage is one per-model row of the usage summary.
type modelUsage struct {
	usageBucket
	Model      string `json:"model"`
	LastUsed   string `json:"last_used,omitempty"`
	LastStatus int    `json:"last_status,omitempty"`
}

// usageSummary is the aggregated response of the management usage endpoints.
type usageSummary struct {
	GeneratedAt  string       `json:"generated_at"`
	Provider     string       `json:"provider"`
	WindowCap    int          `json:"window_cap"`
	Records      int          `json:"records"`
	Streaming    int64        `json:"streaming"`
	NonStreaming int64        `json:"non_streaming"`
	Today        usageBucket  `json:"today"`
	All          usageBucket  `json:"all"`
	Models       []modelUsage `json:"models"`
}

// buildUsageSummary aggregates the in-memory window: an all-time bucket (the
// window), a local-day bucket, and per-model rows. Durable totals remain the
// host's business; this summary describes what this process observed.
func buildUsageSummary() usageSummary {
	entries := snapshotUsageEntries()
	dayStart := time.Now().Truncate(24 * time.Hour)
	summary := usageSummary{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Provider:    providerName,
		WindowCap:   usageStoreCapacity,
		Records:     len(entries),
		Models:      make([]modelUsage, 0),
	}
	byModel := make(map[string]*modelUsage)
	for _, entry := range entries {
		summary.All.add(entry)
		if entry.Stream {
			summary.Streaming++
		} else {
			summary.NonStreaming++
		}
		if ts, err := time.Parse(time.RFC3339Nano, entry.Time); err == nil && ts.After(dayStart) {
			summary.Today.add(entry)
		}
		row := byModel[entry.Model]
		if row == nil {
			row = &modelUsage{}
			byModel[entry.Model] = row
		}
		row.add(entry)
		row.LastUsed = entry.Time
		row.LastStatus = entry.Status
	}
	for model, row := range byModel {
		row.Model = model
		summary.Models = append(summary.Models, *row)
	}
	sort.Slice(summary.Models, func(i, j int) bool {
		if summary.Models[i].Requests != summary.Models[j].Requests {
			return summary.Models[i].Requests > summary.Models[j].Requests
		}
		return summary.Models[i].Model < summary.Models[j].Model
	})
	return summary
}

// usageRecordsResponse is the raw record listing for the panel.
type usageRecordsResponse struct {
	Records []usageEntry `json:"records"`
	Count   int          `json:"count"`
}

// formatUsageLog renders one record as a compact single-line JSON event.
func formatUsageLog(record pluginapi.UsageRecord) string {
	event := usageLogEvent{
		Time:          time.Now().Format(time.RFC3339),
		RequestID:     record.RequestID,
		Provider:      record.Provider,
		Model:         record.Model,
		ResponseModel: record.ResponseModel,
		AuthType:      record.AuthType,
		APIKey:        redactKey(record.APIKey),
		Stream:        record.Stream,
		Failed:        record.Failed,
		LatencyMS:     record.Latency.Milliseconds(),
		InputTokens:   record.Detail.InputTokens,
		OutputTokens:  record.Detail.OutputTokens,
		Reasoning:     record.Detail.ReasoningTokens,
		Cached:        record.Detail.CachedTokens + record.Detail.CacheReadTokens,
		TotalTokens:   record.Detail.TotalTokens,
	}
	if record.TTFT > 0 {
		event.TTFTMS = record.TTFT.Milliseconds()
	}
	if record.Failed {
		event.Status = record.Failure.StatusCode
	}
	out, err := json.Marshal(event)
	if err != nil {
		return fmt.Sprintf(`{"provider":%q,"model":%q,"failed":%v}`,
			record.Provider, record.Model, record.Failed)
	}
	return string(out)
}

// redactKey gives operators a stable client label without exposing key bytes.
func redactKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("client-%x", sum[:6])
}
