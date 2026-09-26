package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func registerWithConfig(t *testing.T, configYAML string) registrationResponse {
	t.Helper()
	request, errMarshal := json.Marshal(map[string][]byte{"config_yaml": []byte(configYAML)})
	if errMarshal != nil {
		t.Fatalf("marshal lifecycle request: %v", errMarshal)
	}
	result, errRegister := registerPlugin(request)
	if errRegister != nil {
		t.Fatalf("registerPlugin: %v", errRegister)
	}
	return result.(registrationResponse)
}

func TestRegisterExposesProviderAndModels(t *testing.T) {
	registration := registerWithConfig(t, "base_url: https://open.bigmodel.cn/api/anthropic/\nmodels:\n  - zcode/glm-5.3\nmodel_map:\n  zcode/glm-5.3: GLM-5.3\n")
	if registration.Metadata.Name == "" || registration.Metadata.Version == "" || registration.Metadata.GitHubRepository == "" {
		t.Fatalf("registration metadata is incomplete: %+v", registration.Metadata)
	}
	if !registration.Capabilities.ModelProvider || !registration.Capabilities.Executor || registration.Capabilities.ExecutorModelScope != "static" {
		t.Fatalf("unexpected capabilities: %+v", registration.Capabilities)
	}
	cfg := activeConfig()
	if cfg.BaseURL != "https://open.bigmodel.cn/api/anthropic" {
		t.Fatalf("base url not normalized: %q", cfg.BaseURL)
	}
	if got := cfg.upstreamModel("zcode/glm-5.3"); got != "GLM-5.3" {
		t.Fatalf("model_map ignored: %q", got)
	}
	if got := cfg.upstreamModel("zcode/glm-5.3-flash"); got != "glm-5.3-flash" {
		t.Fatalf("provider prefix not stripped: %q", got)
	}
	models, errModels := staticModels()
	if errModels != nil {
		t.Fatalf("staticModels: %v", errModels)
	}
	list := models.(modelResponse)
	if len(list.Models) != 1 || list.Models[0].ID != "zcode/glm-5.3" || list.Models[0].Name != "GLM-5.3" {
		t.Fatalf("unexpected models: %+v", list.Models)
	}
}

func TestDefaultModelsAvoidNativeProviderNames(t *testing.T) {
	cfg := config{}.withDefaults()
	for _, id := range cfg.Models {
		if !strings.HasPrefix(id, providerID+"/") {
			t.Fatalf("model %q would collide with native provider models", id)
		}
	}
}

func TestRewriteBodyPinsModelAndStream(t *testing.T) {
	stream := true
	body, errRewrite := rewriteBody([]byte(`{"model":"client-model","stream":false,"messages":[{"role":"user","content":"<b>hi</b>"}]}`), "glm-5.3", &stream)
	if errRewrite != nil {
		t.Fatalf("rewriteBody: %v", errRewrite)
	}
	var doc map[string]any
	if errDecode := json.Unmarshal(body, &doc); errDecode != nil {
		t.Fatalf("decode rewritten body: %v", errDecode)
	}
	if doc["model"] != "glm-5.3" || doc["stream"] != true {
		t.Fatalf("rewritten body = %v", doc)
	}
	if !strings.Contains(string(body), "<b>hi</b>") {
		t.Fatalf("prompt was escaped: %s", body)
	}
}

func TestUpstreamHeadersDisguiseClient(t *testing.T) {
	cred := credential{APIKey: "key.value", BaseURL: "https://open.bigmodel.cn/api/anthropic"}
	client := http.Header{"Anthropic-Beta": []string{"prompt-caching-2024-07-31"}}
	headers := upstreamHeaders(cred, "3.12.3", "darwin-arm64", "macos", "15.6.0", "mid-1234", client)
	expected := map[string]string{
		"X-Api-Key":           "key.value",
		"Authorization":       "Bearer key.value",
		"User-Agent":          "ZCode/3.12.3",
		"X-ZCode-App-Version": "3.12.3",
		"X-Platform":          "darwin-arm64",
		"X-Os-Category":       "macos",
		"X-Os-Version":        "15.6.0",
		"X-Device-Mid":        "mid-1234",
		"HTTP-Referer":        "https://open.bigmodel.cn/",
		"Anthropic-Version":   anthropicVersion,
		"Anthropic-Beta":      "prompt-caching-2024-07-31",
		"X-Release-Channel":   "production",
		"X-Client-Language":   "zh-CN",
		"X-Client-Timezone":   "Asia/Shanghai",
		"X-Title":             "Z Code@electron",
	}
	for name, want := range expected {
		if got := headers.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if headers.Get("X-Request-Id") == "" {
		t.Fatal("X-Request-Id was not set")
	}
	// Headers the client never sends must not appear: the upstream can treat a
	// stray X-Zcode-Agent or X-Zcode-Session-Type as a client that is not ZCode.
	for _, absent := range []string{"X-Zcode-Agent", "X-ZCode-Agent", "X-Zcode-Session-Type", "X-Zcode-Trace-Id"} {
		if headers.Get(absent) != "" {
			t.Fatalf("%s should not be sent", absent)
		}
	}
}

func TestOSVersionAndDeviceMidAreOptional(t *testing.T) {
	cred := credential{APIKey: "k", BaseURL: "https://open.bigmodel.cn/api/anthropic"}
	headers := upstreamHeaders(cred, "3.12.3", "linux-x64", "linux", "", "", nil)
	if headers.Get("X-Os-Version") != "" || headers.Get("X-Device-Mid") != "" {
		t.Fatalf("empty os version / device mid should omit headers: %v", headers)
	}
	if got := headers.Get("HTTP-Referer"); got != "https://open.bigmodel.cn/" {
		t.Fatalf("HTTP-Referer = %q", got)
	}
	if mid := ensureDeviceMid(""); mid == "" {
		t.Fatal("ensureDeviceMid should mint an id when config is empty")
	}
	if mid := ensureDeviceMid("  keep-me  "); mid != "keep-me" {
		t.Fatalf("ensureDeviceMid = %q, want the configured value", mid)
	}
}

// A Linux build must not tell the upstream it is a macOS desktop client.
func TestPlatformDefaultsFollowBuildTarget(t *testing.T) {
	cfg := config{}.withDefaults()
	if cfg.ClientPlatform == "" || cfg.ClientOSCategory == "" {
		t.Fatalf("platform defaults not filled: %+v", cfg)
	}
	if want := clientPlatform(); cfg.ClientPlatform != want {
		t.Fatalf("ClientPlatform = %q, want %q", cfg.ClientPlatform, want)
	}
	override := config{ClientPlatform: "win32-x64", ClientOSCategory: "windows"}.withDefaults()
	if override.ClientPlatform != "win32-x64" || override.ClientOSCategory != "windows" {
		t.Fatalf("explicit platform config was overwritten: %+v", override)
	}
}

func TestExtractUsageFromStreamEventAndBody(t *testing.T) {
	input, output := extractUsage([]byte("event: message_delta\ndata: {\"usage\":{\"input_tokens\":12,\"output_tokens\":34}}\n\n"))
	if input != 12 || output != 34 {
		t.Fatalf("stream usage = %d/%d", input, output)
	}
	input, output = extractUsage([]byte(`{"usage":{"input_tokens":5,"output_tokens":6}}`))
	if input != 5 || output != 6 {
		t.Fatalf("body usage = %d/%d", input, output)
	}
}

func TestCredentialFromAuthFile(t *testing.T) {
	cred, errCredential := credentialFromAuthFile([]byte(`{"type":"zcode","api_key":"abc.def","label":"main"}`))
	if errCredential != nil {
		t.Fatalf("credentialFromAuthFile: %v", errCredential)
	}
	if cred.APIKey != "abc.def" || cred.BaseURL != defaultBaseURL || cred.Label != "main" {
		t.Fatalf("unexpected credential: %+v", cred)
	}
	if _, errEmpty := credentialFromAuthFile([]byte(`{"type":"zcode"}`)); errEmpty == nil {
		t.Fatal("expected an error for an auth file without a key")
	}
	if got := maskKey("0123456789abcdef0123456789abcdef.FAKESECRETVALUE"); got != "01234567...ALUE" {
		t.Fatalf("maskKey = %q", got)
	}
}

func managementCall(t *testing.T, req managementRequest) managementResponse {
	t.Helper()
	payload, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		t.Fatalf("marshal management request: %v", errMarshal)
	}
	result, errHandle := handleManagement(payload)
	if errHandle != nil {
		t.Fatalf("handleManagement: %v", errHandle)
	}
	return result.(managementResponse)
}

func TestManagementRegistersConsoleAndStatus(t *testing.T) {
	registration, errRegister := registerManagement()
	if errRegister != nil {
		t.Fatalf("registerManagement: %v", errRegister)
	}
	payload := registration.(map[string]any)
	routes := payload["routes"].([]managementRoute)
	if len(routes) != 3 || routes[0].Path != "/v0/management/zcode/status" {
		t.Fatalf("unexpected routes: %+v", routes)
	}
	resources := payload["resources"].([]resourceRoute)
	if len(resources) != 1 || resources[0].Path != "/console" || resources[0].Menu == "" {
		t.Fatalf("unexpected resources: %+v", resources)
	}

	page := managementCall(t, managementRequest{Method: http.MethodGet, Path: consoleResourceURL})
	if page.StatusCode != http.StatusOK || !strings.Contains(page.Headers.Get("Content-Type"), "text/html") {
		t.Fatalf("console response = %+v", page)
	}
	if !strings.Contains(string(page.Body), "/v0/management/zcode") {
		t.Fatal("console page does not call the plugin management API")
	}

	status := managementCall(t, managementRequest{Method: http.MethodGet, Path: managementBasePath + "/status"})
	if status.StatusCode != http.StatusOK {
		t.Fatalf("status response = %+v", status)
	}
	var decoded consoleStatus
	if errDecode := json.Unmarshal(status.Body, &decoded); errDecode != nil {
		t.Fatalf("decode status: %v", errDecode)
	}
	if decoded.Provider != providerID || len(decoded.Models) == 0 || decoded.BaseURL == "" {
		t.Fatalf("unexpected status: %+v", decoded)
	}

	if missing := managementCall(t, managementRequest{Method: http.MethodGet, Path: managementBasePath + "/nope"}); missing.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route status = %d", missing.StatusCode)
	}
}

func TestProbeReportsMissingAccount(t *testing.T) {
	result, errProbe := probeAccount([]byte(`{"auth_index":""}`))
	if errProbe != nil {
		t.Fatalf("probeAccount: %v", errProbe)
	}
	var decoded probeResponse
	if errDecode := json.Unmarshal(result.(managementResponse).Body, &decoded); errDecode != nil {
		t.Fatalf("decode probe response: %v", errDecode)
	}
	if decoded.OK || decoded.Detail == "" {
		t.Fatalf("probe response = %+v", decoded)
	}
}

func TestConfigRejectsWrongYAMLType(t *testing.T) {
	var parsed config
	if errYAML := yaml.Unmarshal([]byte("models: not-a-list"), &parsed); errYAML == nil {
		t.Fatal("expected a YAML type error")
	}
}

// The host dlcloses the shared library the moment shutdown returns, so a live
// stream relay must be aborted rather than waited out.
func TestShutdownCancelsLiveStreams(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	streamWG.Add(1)
	trackStream("stream-under-test", cancel)
	go func() {
		defer streamWG.Done()
		defer untrackStream("stream-under-test")
		<-ctx.Done()
	}()

	done := make(chan struct{})
	go func() {
		cliproxyPluginShutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not return promptly after cancelling live streams")
	}
	if ctx.Err() == nil {
		t.Fatal("shutdown left the stream context live")
	}
	streamsMu.Lock()
	remaining := len(streams)
	streamsMu.Unlock()
	if remaining != 0 {
		t.Fatalf("%d streams still tracked after shutdown", remaining)
	}
}
