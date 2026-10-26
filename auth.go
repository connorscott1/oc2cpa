package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ocAuthView is the subset of the selected auth the executor needs. A
// synthetic auth file looks like:
//
//	{"type":"oc2cpa","label":"OpenCode free (anonymous)","proxy_url":"http://user:pass@host:port"}
type ocAuthView struct {
	Holder   string
	ProxyURL string
}

func parseAuthView(storageJSON []byte) ocAuthView {
	view := ocAuthView{Holder: "anonymous"}
	if len(storageJSON) == 0 {
		return view
	}
	var raw map[string]any
	if err := json.Unmarshal(storageJSON, &raw); err != nil {
		return view
	}
	if proxy, ok := raw["proxy_url"].(string); ok && proxy != "" {
		view.ProxyURL = proxy
	}
	return view
}

// handleParseAuth claims auth files typed as this provider. As with well-known
// CPA plugin providers, the plugin leaves Auth.ID empty and echoes FileName so
// the host performs its own per-file upsert and never duplicates records.
func handleParseAuth(requestJSON []byte) pluginapi.AuthParseResponse {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(requestJSON, &req); err != nil {
		return pluginapi.AuthParseResponse{Handled: false}
	}
	var raw map[string]any
	if err := json.Unmarshal(req.RawJSON, &raw); err != nil {
		return pluginapi.AuthParseResponse{Handled: false}
	}
	declaredType, _ := raw["type"].(string)
	if declaredType != providerName {
		return pluginapi.AuthParseResponse{Handled: false}
	}
	storageJSON, _ := json.Marshal(raw)
	return pluginapi.AuthParseResponse{
		Handled: true,
		Auth: pluginapi.AuthData{
			Provider:    providerName,
			FileName:    req.FileName,
			Label:       authLabel(raw),
			StorageJSON: storageJSON,
			Metadata: map[string]any{
				"type":     providerName,
				"provider": providerName,
			},
		},
	}
}

func authLabel(raw map[string]any) string {
	if label, ok := raw["label"].(string); ok && label != "" {
		return label
	}
	return "OpenCode free models (anonymous)"
}

// handleRefreshAuth keeps the auth permanently valid: the free tier uses no
// expiring credentials, so the refresh call echoes the stored payload back
// without scheduling another refresh cycle.
func handleRefreshAuth(requestJSON []byte) pluginapi.AuthRefreshResponse {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(requestJSON, &req); err != nil {
		return pluginapi.AuthRefreshResponse{}
	}
	return pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider:    providerName,
			StorageJSON: req.StorageJSON,
			Metadata: map[string]any{
				"type":     providerName,
				"provider": providerName,
			},
		},
	}
}

// hostAuthListEntry is the wire payload for host.auth.list. The plugin only
// needs the file names and provider fields for bootstrap detection.
type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

var bootstrapMu sync.Mutex

// bootstrapAuthOnce creates the synthetic auth file on first registration so
// the plugin is usable right after the two-line config.yaml addition. Failure
// is non-fatal: the user can also drop an oc2cpa.json into auth-dir manually.
func bootstrapAuthOnce() {
	if err := bootstrapAuth(goHostCallJson); err != nil {
		fsDebugLog("synthetic credential setup failed; existing files were not intentionally replaced")
	}
}

// The host callback returns an envelope; callers must inspect result.files.
// Serialize our own registrations and never save over an existing name, even
// when the existing file has a different provider or is disabled.
func bootstrapAuth(call func(string, any) (json.RawMessage, error)) error {
	bootstrapMu.Lock()
	defer bootstrapMu.Unlock()
	responseBytes, err := call(pluginabi.MethodHostAuthList, struct{}{})
	if err != nil {
		return err
	}
	var list hostAuthListResponse
	if err := json.Unmarshal(responseBytes, &list); err != nil {
		return err
	}
	if list.Files == nil {
		return fmt.Errorf("host credential list is missing files")
	}
	for _, file := range list.Files {
		if file.Provider == providerName || file.Type == providerName || strings.EqualFold(filepath.Base(file.Name), providerName+".json") {
			return nil
		}
	}
	payload, _ := json.Marshal(map[string]any{
		"type":      providerName,
		"provider":  providerName,
		"label":     "OpenCode free models (anonymous)",
		"proxy_url": "",
	})
	_, err = call(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{
		Name: providerName + ".json",
		JSON: json.RawMessage(payload),
	})
	return err
}
