package main

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// providerName is the stable identity of this provider: it is the executor and
// auth provider key, the ModelResponse.Provider value, and the auth file "type"
// this plugin recognizes.
const providerName = "oc2cpa"

// version is overridden at build time via -ldflags "-X main.version=x.y.z".
var version = "0.0.0"
var sourceCommit = "development"

var settingsMu sync.RWMutex
var settings = pluginSettings{
	UpstreamBaseURL:  "https://opencode.ai/inference/v1",
	ConnectTimeoutMS: 60_000,
	RequestTimeoutMS: 600_000,
	DynamicModels:    false,
	UsageLog:         true,
}

// pluginSettings holds the plugin-owned configuration keys under
// plugins.configs.oc2cpa in config.yaml. The host passes the section (plus
// enabled/priority) as YAML to plugin.register/plugin.reconfigure.
type pluginSettings struct {
	UpstreamBaseURL  string
	ConnectTimeoutMS int
	RequestTimeoutMS int
	DynamicModels    bool
	UsageLog         bool
}

func configure(requestJSON []byte) {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if err := json.Unmarshal(requestJSON, &req); err != nil {
		return
	}
	parsed := parseSettings(req.ConfigYAML)
	settingsMu.Lock()
	settings = parsed
	settingsMu.Unlock()
	invalidateModelCache()
}

func readSettings() pluginSettings {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return settings
}

// configLineRe matches one top-level "key: value" line of the plugin section.
var configLineRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_]*)\s*:\s*(.*)$`)

// parseSettings does a deliberately minimal line-based read of the YAML section.
// Supported keys: upstream_base_url (string), connect_timeout_ms (int),
// dynamic_models (bool), usage_log (bool). Only top-level "key: value" lines
// are considered; the section is flat, so this avoids pulling a YAML
// dependency into the plugin.
func parseSettings(configYAML []byte) pluginSettings {
	parsed := pluginSettings{
		UpstreamBaseURL:  "https://opencode.ai/inference/v1",
		ConnectTimeoutMS: 60_000,
		RequestTimeoutMS: 600_000,
		DynamicModels:    false,
		UsageLog:         true,
	}
	for line := range strings.SplitSeq(strings.ReplaceAll(string(configYAML), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		match := configLineRe.FindStringSubmatch(trimmed)
		if match == nil {
			continue
		}
		key, value := match[1], strings.TrimSpace(match[2])
		value = strings.Trim(value, `"'`)
		if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		switch key {
		case "upstream_base_url":
			if value != "" {
				parsed.UpstreamBaseURL = strings.TrimRight(value, "/")
			}
		case "connect_timeout_ms":
			if n, err := strconv.Atoi(value); err == nil {
				parsed.ConnectTimeoutMS = clampConnectTimeout(n)
			}
		case "request_timeout_ms":
			if n, err := strconv.Atoi(value); err == nil {
				parsed.RequestTimeoutMS = max(1, min(n, 1_800_000))
			}
		case "dynamic_models":
			parsed.DynamicModels = value == "true"
		case "usage_log":
			parsed.UsageLog = value == "" || value == "true"
		}
	}
	if parsed.ConnectTimeoutMS == 0 {
		parsed.ConnectTimeoutMS = 60_000
	}
	return parsed
}

// clampConnectTimeout bounds the connect timeout the same way the original
// server validated ZEN_CONNECT_TIMEOUT_MS (1..600000 ms).
func clampConnectTimeout(ms int) int {
	if ms < 1 {
		return 1
	}
	if ms > 600_000 {
		return 600_000
	}
	return ms
}

// registrationPayload is the plugin.register response payload. The host
// requires non-empty metadata and at least one capability.
type registrationPayload struct {
	BuildCommit   string             `json:"build_commit,omitempty"`
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  map[string]any     `json:"capabilities"`
}

func registration() registrationPayload {
	return registrationPayload{
		BuildCommit: sourceCommit,
		// Plugin RPC schema contract version (see sdk/pluginabi/types.go):
		// 6 preserves raw JSON for management responses; the plugin does not
		// rely on per-chunk request bodies or history chunks.
		SchemaVersion: 6,
		Metadata: pluginapi.Metadata{
			Name:             "OC2CPA (OpenCode free models)",
			Version:          version,
			Author:           "connorscott1 (fork of dingguotu/oc2cpa)",
			GitHubRepository: "https://github.com/connorscott1/oc2cpa",
			Logo:             "https://opencode.ai/favicon.svg",
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "upstream_base_url",
					Type:        pluginapi.ConfigFieldTypeString,
					Default:     "https://opencode.ai/inference/v1",
					Description: "OpenCode inference endpoint base URL; a local relay can be put here",
				},
				{
					Name:        "connect_timeout_ms",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Default:     60000,
					Description: "Milliseconds to wait for the upstream response headers before giving up (1..600000)",
				},
				{
					Name:        "request_timeout_ms",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Default:     600000,
					Description: "Total upstream request deadline including response body (1..1800000 ms)",
				},
				{
					Name:        "dynamic_models",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Default:     false,
					Description: "Discover free models from the upstream /models endpoint in addition to the built-in catalog",
				},
				{
					Name:        "usage_log",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Default:     true,
					Description: "Emit a per-request forwarding log (model, latency, token usage) into the host log",
				},
			},
		},
		Capabilities: map[string]any{
			"model_provider":          true,
			"auth_provider":           true,
			"executor":                true,
			"executor_model_scope":    "both",
			"executor_input_formats":  []string{"chat-completions"},
			"executor_output_formats": []string{"chat-completions"},
			"management_api":          true,
			"usage_plugin":            true,
			"scheduler":               false,
			"frontend_auth_provider":  false,
		},
	}
}
