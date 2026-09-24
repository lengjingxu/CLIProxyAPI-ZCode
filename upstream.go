package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	messagesEndpoint     = "/v1/messages"
	countTokensEndpoint  = "/v1/messages/count_tokens"
	eventStreamMediaType = "text/event-stream"
)

var upstreamHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		MaxIdleConns:      32,
		IdleConnTimeout:   90 * time.Second,
		ForceAttemptHTTP2: true,
	},
}

func requestContext(cfg config) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Duration(cfg.RequestTimeout)*time.Second)
}

func newUUID() string {
	buf := make([]byte, 16)
	if _, errRead := rand.Read(buf); errRead != nil {
		return fmt.Sprintf("zcode-%d", time.Now().UnixNano())
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(buf[0:4]),
		hex.EncodeToString(buf[4:6]),
		hex.EncodeToString(buf[6:8]),
		hex.EncodeToString(buf[8:10]),
		hex.EncodeToString(buf[10:16]),
	)
}

// encodeJSON avoids HTML escaping so prompts survive a body rewrite unchanged.
func encodeJSON(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if errEncode := encoder.Encode(value); errEncode != nil {
		return nil, errEncode
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// refererFor reproduces the client's HTTP-Referer, which is the origin of the
// endpoint the request is going to rather than a fixed site.
func refererFor(baseURL string) string {
	parsed, errParse := url.Parse(baseURL)
	if errParse != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "https://zcode.z.ai/"
	}
	return parsed.Scheme + "://" + parsed.Host + "/"
}

// upstreamHeaders mirrors the header set the ZCode desktop client builds in
// buildZCodeSourceHeaders: the source headers plus the transport headers the
// Anthropic path adds. Names and casing follow the client so the two are
// indistinguishable on the wire.
func upstreamHeaders(cred credential, clientVersion, platform, osCategory, osVersion, deviceMid string, clientHeaders http.Header) http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set("X-Api-Key", cred.APIKey)
	headers.Set("Authorization", "Bearer "+cred.APIKey)
	headers.Set("Anthropic-Version", anthropicVersion)
	headers.Set("User-Agent", "ZCode/"+clientVersion)
	headers.Set("HTTP-Referer", refererFor(cred.BaseURL))
	headers.Set("X-Title", "Z Code@electron")
	headers.Set("X-ZCode-App-Version", clientVersion)
	headers.Set("X-Release-Channel", "production")
	headers.Set("X-Client-Language", "zh-CN")
	headers.Set("X-Client-Timezone", "Asia/Shanghai")
	headers.Set("X-Platform", platform)
	headers.Set("X-Os-Category", osCategory)
	if osVersion != "" {
		headers.Set("X-Os-Version", osVersion)
	}
	if deviceMid != "" {
		headers.Set("X-Device-Mid", deviceMid)
	}
	headers.Set("X-Request-Id", newUUID())
	if clientHeaders != nil {
		if beta := strings.TrimSpace(clientHeaders.Get("Anthropic-Beta")); beta != "" {
			headers.Set("Anthropic-Beta", beta)
		}
	}
	return headers
}

func (c credential) post(ctx context.Context, endpoint string, headers http.Header, body []byte) (*http.Response, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+endpoint, bytes.NewReader(body))
	if errRequest != nil {
		return nil, errRequest
	}
	req.Header = headers
	req.ContentLength = int64(len(body))
	return upstreamHTTPClient.Do(req)
}

// rewriteBody pins the model and stream flag that the host resolved, leaving the
// rest of the client payload untouched.
func rewriteBody(payload []byte, model string, stream *bool) ([]byte, error) {
	if len(payload) == 0 {
		return nil, failf("invalid_request", http.StatusBadRequest, "empty request body")
	}
	var doc map[string]any
	if errDecode := json.Unmarshal(payload, &doc); errDecode != nil {
		return nil, failf("invalid_request", http.StatusBadRequest, "request body is not a JSON object: %v", errDecode)
	}
	if model != "" {
		doc["model"] = model
	}
	if stream != nil {
		doc["stream"] = *stream
	}
	return encodeJSON(doc)
}

func upstreamError(status int, body []byte) *pluginError {
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(status)
	}
	return &pluginError{
		code:    "upstream_error",
		status:  status,
		message: fmt.Sprintf("zcode upstream %d: %s", status, truncateRunes(message, 600)),
	}
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}

func forwardedResponseHeaders(header http.Header, fallbackContentType string) http.Header {
	out := http.Header{}
	contentType := ""
	if header != nil {
		contentType = strings.TrimSpace(header.Get("Content-Type"))
	}
	if contentType == "" {
		contentType = fallbackContentType
	}
	if contentType != "" {
		out.Set("Content-Type", contentType)
	}
	return out
}

func emitStreamChunk(streamID string, payload []byte) error {
	return hostCall(methodHostStreamEmit, map[string]any{"stream_id": streamID, "payload": payload}, nil)
}

func closeDownstreamStream(streamID, errMessage string) {
	request := map[string]any{"stream_id": streamID}
	if errMessage != "" {
		request["error"] = errMessage
	}
	_ = hostCall(methodHostStreamClose, request, nil)
}

// pumpStream forwards complete SSE events upstream-to-client and closes the
// host stream when the upstream body ends. ctx carries the request deadline
// and is cancelled early when the host shuts the plugin down.
func pumpStream(ctx context.Context, streamID string, body io.Reader, model string) {
	reader := bufio.NewReaderSize(body, 32*1024)
	var pending []byte
	var errMessage string
	var inputTokens, outputTokens int64
	for {
		line, errRead := reader.ReadBytes('\n')
		if len(line) > 0 {
			pending = append(pending, line...)
			if isBlankLine(line) {
				event := pending
				pending = nil
				in, out := extractUsage(event)
				if in > inputTokens {
					inputTokens = in
				}
				if out > outputTokens {
					outputTokens = out
				}
				if errEmit := emitStreamChunk(streamID, event); errEmit != nil {
					errMessage = errEmit.Error()
					break
				}
			}
		}
		if errRead != nil {
			if errRead != io.EOF && !errors.Is(errRead, context.Canceled) {
				errMessage = errRead.Error()
			}
			break
		}
	}
	if ctx.Err() != nil {
		pending = nil
	}
	if errMessage == "" && len(pending) > 0 {
		in, out := extractUsage(pending)
		if in > inputTokens {
			inputTokens = in
		}
		if out > outputTokens {
			outputTokens = out
		}
		if errEmit := emitStreamChunk(streamID, pending); errEmit != nil {
			errMessage = errEmit.Error()
		}
	}
	stats.record(model, inputTokens, outputTokens, errMessage)
	closeDownstreamStream(streamID, errMessage)
}

func isBlankLine(line []byte) bool {
	return len(strings.TrimRight(string(line), "\r\n")) == 0
}

type usageBlock struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// extractUsage reads Anthropic usage counters from a JSON body or SSE event.
func extractUsage(payload []byte) (int64, int64) {
	start := bytes.IndexByte(payload, '{')
	if start < 0 {
		return 0, 0
	}
	var doc struct {
		Usage   *usageBlock `json:"usage"`
		Message *struct {
			Usage *usageBlock `json:"usage"`
		} `json:"message"`
	}
	if errDecode := json.Unmarshal(bytes.TrimSpace(payload[start:]), &doc); errDecode != nil {
		return 0, 0
	}
	usage := doc.Usage
	if usage == nil && doc.Message != nil {
		usage = doc.Message.Usage
	}
	if usage == nil {
		return 0, 0
	}
	return usage.InputTokens, usage.OutputTokens
}

func maskKey(key string) string {
	if len(key) <= 12 {
		return "****"
	}
	return key[:8] + "..." + key[len(key)-4:]
}

// probeCredential validates a key cheaply through the count_tokens endpoint.
func probeCredential(cred credential, cfg config) (int, string) {
	model := "glm-4.6"
	if len(cfg.Models) > 0 {
		model = cfg.upstreamModel(cfg.Models[0])
	}
	body, errEncode := encodeJSON(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "ping"}},
	})
	if errEncode != nil {
		return 0, errEncode.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, errPost := cred.post(ctx, countTokensEndpoint, upstreamHeaders(cred, cfg.ClientVersion, cfg.ClientPlatform, cfg.ClientOSCategory, cfg.ClientOSVersion, ensureDeviceMid(cfg.DeviceMid), nil), body)
	if errPost != nil {
		return 0, errPost.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, truncateRunes(strings.TrimSpace(string(raw)), 200)
}
