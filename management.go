package main

import (
	"embed"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

//go:embed ui/index.html
var uiFiles embed.FS

const (
	managementBasePath = "/v0/management/" + providerID
	consoleResourceURL = "/v0/resource/plugins/" + providerID + "/console"
)

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

func registerManagement() (any, error) {
	return map[string]any{
		"routes": []managementRoute{
			{Method: http.MethodGet, Path: managementBasePath + "/status"},
			{Method: http.MethodGet, Path: managementBasePath + "/models"},
			{Method: http.MethodPost, Path: managementBasePath + "/probe"},
		},
		"resources": []resourceRoute{
			{Path: "/console", Menu: "ZCode", Description: "ZCode 反代：额度消耗、模型与账号状态"},
		},
	}, nil
}

// managementRequest mirrors the host side management.handle payload.
type managementRequest struct {
	Method         string      `json:"Method"`
	Path           string      `json:"Path"`
	Headers        http.Header `json:"Headers"`
	Query          url.Values  `json:"Query"`
	Body           []byte      `json:"Body"`
	HostCallbackID string      `json:"host_callback_id"`
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

func jsonManagementResponse(status int, value any) (any, error) {
	body, errEncode := encodeJSON(value)
	if errEncode != nil {
		return nil, errEncode
	}
	return managementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}},
		Body:       body,
	}, nil
}

type accountStatus struct {
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name"`
	Label     string `json:"label,omitempty"`
	Status    string `json:"status,omitempty"`
	KeyMask   string `json:"key_mask,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
}

type consoleStatus struct {
	Provider       string           `json:"provider"`
	BaseURL        string           `json:"base_url"`
	ClientVersion  string           `json:"client_version"`
	RequestTimeout int              `json:"request_timeout"`
	Models         []modelInfo      `json:"models"`
	Counters       countersSnapshot `json:"counters"`
	Accounts       []accountStatus  `json:"accounts"`
}

func handleManagement(request []byte) (any, error) {
	var req managementRequest
	if len(request) > 0 {
		if errDecode := json.Unmarshal(request, &req); errDecode != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "invalid management request"})
		}
	}
	if req.Method == http.MethodGet && req.Path == consoleResourceURL {
		page, errRead := uiFiles.ReadFile("ui/index.html")
		if errRead != nil {
			return jsonManagementResponse(http.StatusInternalServerError, map[string]string{"error": errRead.Error()})
		}
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}, "Cache-Control": []string{"no-store"}},
			Body:       page,
		}, nil
	}
	switch req.Method + " " + req.Path {
	case "GET " + managementBasePath + "/status":
		return jsonManagementResponse(http.StatusOK, buildConsoleStatus())
	case "GET " + managementBasePath + "/models":
		return staticModels()
	case "POST " + managementBasePath + "/probe":
		return probeAccount(req.Body)
	default:
		return jsonManagementResponse(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func buildConsoleStatus() consoleStatus {
	cfg := activeConfig()
	models, _ := staticModels()
	return consoleStatus{
		Provider:       providerID,
		BaseURL:        cfg.BaseURL,
		ClientVersion:  cfg.ClientVersion,
		RequestTimeout: cfg.RequestTimeout,
		Models:         models.(modelResponse).Models,
		Counters:       stats.snapshot(),
		Accounts:       listAccounts(),
	}
}

// listAccounts reports the credential files the host holds for this provider,
// with each key reduced to a non-recoverable fragment.
func listAccounts() []accountStatus {
	var listed struct {
		Files []struct {
			AuthIndex string `json:"auth_index"`
			Name      string `json:"name"`
			Label     string `json:"label"`
			Type      string `json:"type"`
			Provider  string `json:"provider"`
			Status    string `json:"status"`
			Disabled  bool   `json:"disabled"`
		} `json:"files"`
	}
	if errList := hostCall(methodHostAuthList, nil, &listed); errList != nil {
		return nil
	}
	accounts := make([]accountStatus, 0, len(listed.Files))
	for _, file := range listed.Files {
		if !isZCodeAuth(file.Type, file.Provider, file.Name) {
			continue
		}
		account := accountStatus{
			AuthIndex: file.AuthIndex,
			Name:      file.Name,
			Label:     file.Label,
			Status:    file.Status,
			Disabled:  file.Disabled,
		}
		if cred, errCredential := loadAccountCredential(file.AuthIndex); errCredential == nil {
			account.KeyMask = maskKey(cred.APIKey)
			account.BaseURL = cred.BaseURL
		}
		accounts = append(accounts, account)
	}
	return accounts
}

func isZCodeAuth(values ...string) bool {
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), providerID) {
			return true
		}
	}
	return false
}

func loadAccountCredential(authIndex string) (credential, error) {
	if strings.TrimSpace(authIndex) == "" {
		return credential{}, failf("invalid_request", 0, "auth_index is required")
	}
	var got struct {
		JSON json.RawMessage `json:"json"`
	}
	if errGet := hostCall(methodHostAuthGet, map[string]string{"auth_index": authIndex}, &got); errGet != nil {
		return credential{}, errGet
	}
	return credentialFromAuthFile(got.JSON)
}

type probeRequest struct {
	AuthIndex string `json:"auth_index"`
}

type probeResponse struct {
	AuthIndex string `json:"auth_index"`
	OK        bool   `json:"ok"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
}

// probeAccount spends one count_tokens call to prove the key still works.
func probeAccount(body []byte) (any, error) {
	var req probeRequest
	if len(body) > 0 {
		if errDecode := json.Unmarshal(body, &req); errDecode != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "invalid probe body"})
		}
	}
	cred, errCredential := loadAccountCredential(req.AuthIndex)
	if errCredential != nil {
		return jsonManagementResponse(http.StatusOK, probeResponse{AuthIndex: req.AuthIndex, Detail: errCredential.Error()})
	}
	status, detail := probeCredential(cred, activeConfig())
	return jsonManagementResponse(http.StatusOK, probeResponse{
		AuthIndex: req.AuthIndex,
		OK:        status == http.StatusOK,
		Status:    status,
		Detail:    detail,
	})
}
