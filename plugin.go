package main

import (
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

const (
	providerID            = "zcode"
	pluginName            = "ZCode Reverse Proxy"
	pluginVersion         = "0.1.0"
	pluginAuthor          = "lengjingxu"
	pluginRepository      = "https://github.com/lengjingxu/cpa-plugin-zcode"
	anthropicVersion      = "2023-06-01"
	defaultBaseURL        = "https://api.z.ai/api/anthropic"
	defaultClientVersion  = "3.12.3"
	defaultTimeoutSeconds = 600
)

// defaultModels are registered as <provider>/<model> so they never collide with
// the same model names served by a native CPA provider.
var defaultModels = []string{"zcode/glm-5.3", "zcode/glm-5.3-flash"}

type config struct {
	BaseURL          string            `yaml:"base_url"`
	Models           []string          `yaml:"models"`
	ModelMap         map[string]string `yaml:"model_map"`
	RequestTimeout   int               `yaml:"request_timeout"`
	ClientVersion    string            `yaml:"client_version"`
	ClientPlatform   string            `yaml:"client_platform"`
	ClientOSCategory string            `yaml:"client_os_category"`
	ClientOSVersion  string            `yaml:"client_os_version"`
	DeviceMid        string            `yaml:"device_mid"`
}

func (c config) withDefaults() config {
	if strings.TrimSpace(c.BaseURL) == "" {
		c.BaseURL = defaultBaseURL
	}
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if len(c.Models) == 0 {
		c.Models = append([]string(nil), defaultModels...)
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = defaultTimeoutSeconds
	}
	if strings.TrimSpace(c.ClientVersion) == "" {
		c.ClientVersion = defaultClientVersion
	}
	if strings.TrimSpace(c.ClientPlatform) == "" {
		c.ClientPlatform = clientPlatform()
	}
	if strings.TrimSpace(c.ClientOSCategory) == "" {
		c.ClientOSCategory = clientOSCategory()
	}
	return c
}

// defaultOSVersion is a plausible macOS build so a macOS-shaped request does not
// advertise an empty OS version. Unused on other platforms, where the header is
// simply omitted.
const defaultOSVersion = "15.6.0"

// ensureDeviceMid keeps the configured device_mid, or mints one when the config
// cannot provide it (the host ignores unknown field names, so a missing value
// must not leave the header empty when a client would always send it).
func ensureDeviceMid(value string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return newUUID()
}

// clientPlatform reports the platform tag the ZCode client would send for the
// build this plugin was compiled for, so a Linux or Windows binary does not
// advertise a macOS transport.
func clientPlatform() string {
	name := runtime.GOOS
	if name == "windows" {
		name = "win32"
	}
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x64"
	case "386":
		arch = "ia32"
	}
	return name + "-" + arch
}

func clientOSCategory() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	default:
		return runtime.GOOS
	}
}

// upstreamModel maps a registered model id to the name sent upstream.
func (c config) upstreamModel(id string) string {
	if mapped, ok := c.ModelMap[id]; ok && strings.TrimSpace(mapped) != "" {
		return strings.TrimSpace(mapped)
	}
	if index := strings.LastIndex(id, "/"); index >= 0 && index+1 < len(id) {
		return id[index+1:]
	}
	return id
}

var (
	configMu   sync.RWMutex
	currentCfg = config{}.withDefaults()
)

func activeConfig() config {
	configMu.RLock()
	defer configMu.RUnlock()
	return currentCfg
}

func setConfig(next config) {
	configMu.Lock()
	currentCfg = next
	configMu.Unlock()
}

type registrationResponse struct {
	SchemaVersion uint32           `json:"schema_version"`
	Metadata      registrationMeta `json:"metadata"`
	Capabilities  registrationCaps `json:"capabilities"`
}

type registrationMeta struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo"`
	ConfigFields     []configField `json:"ConfigFields"`
}

type configField struct {
	Name        string `json:"Name"`
	Type        string `json:"Type"`
	Description string `json:"Description"`
}

type registrationCaps struct {
	ModelProvider         bool     `json:"model_provider"`
	AuthProvider          bool     `json:"auth_provider"`
	Executor              bool     `json:"executor"`
	ManagementAPI         bool     `json:"management_api"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

func registerPlugin(request []byte) (any, error) {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if len(request) > 0 {
		if errDecode := json.Unmarshal(request, &req); errDecode != nil {
			return nil, failf("invalid_request", 0, "decode lifecycle request: %v", errDecode)
		}
	}
	parsed := config{}
	if len(req.ConfigYAML) > 0 {
		if errYAML := yaml.Unmarshal(req.ConfigYAML, &parsed); errYAML != nil {
			return nil, failf("invalid_config", 0, "parse zcode plugin config: %v", errYAML)
		}
	}
	setConfig(parsed.withDefaults())
	return registrationPayload(), nil
}

func registrationPayload() registrationResponse {
	return registrationResponse{
		SchemaVersion: 1,
		Metadata: registrationMeta{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepository,
			ConfigFields: []configField{
				{Name: "base_url", Type: "string", Description: "上游 Anthropic 兼容基址，留空使用 " + defaultBaseURL},
				{Name: "models", Type: "array", Description: "注册给客户端调用的模型 ID 列表，例如 zcode/glm-5.3"},
				{Name: "model_map", Type: "object", Description: "模型 ID 到上游模型名的覆盖映射"},
				{Name: "request_timeout", Type: "integer", Description: "上游请求超时秒数，默认 600"},
				{Name: "client_version", Type: "string", Description: "伪装 ZCode 客户端版本，默认 " + defaultClientVersion},
				{Name: "client_platform", Type: "string", Description: "X-Platform 伪装值，留空按编译目标平台"},
				{Name: "client_os_category", Type: "string", Description: "X-Os-Category 伪装值，留空按编译目标平台"},
				{Name: "client_os_version", Type: "string", Description: "X-Os-Version 伪装值，留空则不发送该头"},
				{Name: "device_mid", Type: "string", Description: "X-Device-Mid 伪装值，留空则每次请求随机生成"},
			},
		},
		Capabilities: registrationCaps{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ManagementAPI:         true,
			ExecutorModelScope:    "static",
			ExecutorInputFormats:  []string{"anthropic"},
			ExecutorOutputFormats: []string{"anthropic"},
		},
	}
}

type modelResponse struct {
	Provider string      `json:"Provider"`
	Models   []modelInfo `json:"Models"`
}

type modelInfo struct {
	ID                        string   `json:"ID"`
	Object                    string   `json:"Object,omitempty"`
	OwnedBy                   string   `json:"OwnedBy,omitempty"`
	Type                      string   `json:"Type,omitempty"`
	DisplayName               string   `json:"DisplayName,omitempty"`
	Name                      string   `json:"Name,omitempty"`
	Description               string   `json:"Description,omitempty"`
	SupportedInputModalities  []string `json:"SupportedInputModalities,omitempty"`
	SupportedOutputModalities []string `json:"SupportedOutputModalities,omitempty"`
}

func staticModels() (any, error) {
	cfg := activeConfig()
	models := make([]modelInfo, 0, len(cfg.Models))
	for _, id := range cfg.Models {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		models = append(models, modelInfo{
			ID:                        id,
			Object:                    "model",
			OwnedBy:                   providerID,
			Type:                      "chat",
			DisplayName:               id,
			Name:                      cfg.upstreamModel(id),
			Description:               "ZCode 客户端额度，上游模型 " + cfg.upstreamModel(id),
			SupportedInputModalities:  []string{"text"},
			SupportedOutputModalities: []string{"text"},
		})
	}
	return modelResponse{Provider: providerID, Models: models}, nil
}

type authParseRequest struct {
	Provider string            `json:"Provider"`
	Path     string            `json:"Path"`
	FileName string            `json:"FileName"`
	RawJSON  []byte            `json:"RawJSON"`
	Host     hostConfigSummary `json:"Host"`
}

type hostConfigSummary struct {
	AuthDir  string `json:"AuthDir"`
	ProxyURL string `json:"ProxyURL"`
}

type authRefreshRequest struct {
	AuthID       string            `json:"AuthID"`
	AuthProvider string            `json:"AuthProvider"`
	StorageJSON  []byte            `json:"StorageJSON"`
	Host         hostConfigSummary `json:"Host"`
}

type authParseResponse struct {
	Handled bool     `json:"Handled"`
	Auth    authData `json:"Auth"`
}

type authRefreshResponse struct {
	Auth authData `json:"Auth"`
}

type authData struct {
	Provider    string            `json:"Provider,omitempty"`
	FileName    string            `json:"FileName,omitempty"`
	Label       string            `json:"Label,omitempty"`
	StorageJSON []byte            `json:"StorageJSON,omitempty"`
	Attributes  map[string]string `json:"Attributes,omitempty"`
}

type credential struct {
	APIKey  string
	BaseURL string
	Label   string
}

// credentialFromAuthFile reads the plugin-owned auth file. APIKey holds the long
// lived Z.AI / BigModel api key (apiKeyId.secretKey) that the ZCode client itself
// uses for the coding plan endpoint.
func credentialFromAuthFile(raw []byte) (credential, error) {
	if len(raw) == 0 {
		return credential{}, errors.New("auth storage is empty")
	}
	var doc map[string]any
	if errDecode := json.Unmarshal(raw, &doc); errDecode != nil {
		return credential{}, errors.New("auth file is not a JSON object")
	}
	cred := credential{
		APIKey:  firstScalar(doc, "api_key", "apiKey", "api-key", "apikey", "key", "token"),
		BaseURL: strings.TrimRight(firstScalar(doc, "base_url", "baseUrl", "base-url"), "/"),
		Label:   firstScalar(doc, "label", "name"),
	}
	if cred.APIKey == "" {
		return credential{}, errors.New("auth file has no api_key field")
	}
	if cred.BaseURL == "" {
		cred.BaseURL = defaultBaseURL
	}
	return cred, nil
}

func firstScalar(doc map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := doc[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func parseAuth(request []byte) (any, error) {
	var req authParseRequest
	if errDecode := json.Unmarshal(request, &req); errDecode != nil {
		return nil, failf("invalid_request", 0, "decode auth parse request: %v", errDecode)
	}
	if !strings.EqualFold(strings.TrimSpace(req.Provider), providerID) {
		return authParseResponse{Handled: false}, nil
	}
	cred, errCredential := credentialFromAuthFile(req.RawJSON)
	if errCredential != nil {
		return nil, failf("invalid_auth", 0, "zcode auth file %s: %v", req.FileName, errCredential)
	}
	label := cred.Label
	if label == "" {
		label = req.FileName
	}
	return authParseResponse{
		Handled: true,
		Auth: authData{
			Provider:    providerID,
			FileName:    req.FileName,
			Label:       label,
			StorageJSON: req.RawJSON,
		},
	}, nil
}

// refreshAuth returns the stored material unchanged: api keys do not expire, so
// the only work is validating that the file still carries a key.
func refreshAuth(request []byte) (any, error) {
	var req authRefreshRequest
	if errDecode := json.Unmarshal(request, &req); errDecode != nil {
		return nil, failf("invalid_request", 0, "decode auth refresh request: %v", errDecode)
	}
	if !strings.EqualFold(strings.TrimSpace(req.AuthProvider), providerID) {
		return authRefreshResponse{}, nil
	}
	if _, errCredential := credentialFromAuthFile(req.StorageJSON); errCredential != nil {
		return nil, failf("invalid_auth", 401, "zcode auth %s: %v", req.AuthID, errCredential)
	}
	return authRefreshResponse{Auth: authData{Provider: providerID, StorageJSON: req.StorageJSON}}, nil
}
