package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type executorRequest struct {
	AuthID          string      `json:"AuthID"`
	AuthProvider    string      `json:"AuthProvider"`
	Model           string      `json:"Model"`
	Format          string      `json:"Format"`
	Stream          bool        `json:"Stream"`
	Headers         http.Header `json:"Headers"`
	Payload         []byte      `json:"Payload"`
	StorageJSON     []byte      `json:"StorageJSON"`
	SourceFormat    string      `json:"SourceFormat"`
	OriginalRequest []byte      `json:"OriginalRequest"`
	StreamID        string      `json:"stream_id"`
}

type executorResponse struct {
	Payload []byte      `json:"Payload,omitempty"`
	Headers http.Header `json:"Headers,omitempty"`
}

type executorStreamResponse struct {
	Headers http.Header `json:"Headers,omitempty"`
}

func prepareExecutorRequest(request []byte) (executorRequest, config, credential, error) {
	var req executorRequest
	if errDecode := json.Unmarshal(request, &req); errDecode != nil {
		return req, config{}, credential{}, failf("invalid_request", 0, "decode executor request: %v", errDecode)
	}
	cfg := activeConfig()
	cred, errCredential := credentialFromAuthFile(req.StorageJSON)
	if errCredential != nil {
		return req, cfg, credential{}, failf("invalid_auth", http.StatusUnauthorized, "zcode auth %s: %v", req.AuthID, errCredential)
	}
	return req, cfg, cred, nil
}

func execute(request []byte) (any, error) {
	req, cfg, cred, errPrepare := prepareExecutorRequest(request)
	if errPrepare != nil {
		return nil, errPrepare
	}
	stream := req.Stream
	body, errBody := rewriteBody(req.Payload, cfg.upstreamModel(req.Model), &stream)
	if errBody != nil {
		return nil, errBody
	}
	ctx, cancel := requestContext(cfg)
	defer cancel()
	resp, errPost := cred.post(ctx, messagesEndpoint, upstreamHeaders(cred, cfg.ClientVersion, cfg.ClientPlatform, cfg.ClientOSCategory, cfg.ClientOSVersion, ensureDeviceMid(cfg.DeviceMid), req.Headers), body)
	if errPost != nil {
		stats.recordFailure(req.Model, errPost.Error())
		return nil, failf("upstream_unreachable", http.StatusBadGateway, "zcode upstream: %v", errPost)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		stats.recordFailure(req.Model, errRead.Error())
		return nil, failf("upstream_read_failed", http.StatusBadGateway, "read zcode response: %v", errRead)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		stats.recordFailure(req.Model, strings.TrimSpace(string(raw)))
		return nil, upstreamError(resp.StatusCode, raw)
	}
	hostLog(fmt.Sprintf("zcode upstream model=%s stream=%t status=%d bytes_in=%d bytes_out=%d", req.Model, req.Stream, resp.StatusCode, len(body), len(raw)))
	inputTokens, outputTokens := extractUsage(raw)
	stats.record(req.Model, inputTokens, outputTokens, "")
	return executorResponse{Payload: raw, Headers: forwardedResponseHeaders(resp.Header, "application/json")}, nil
}

func countTokens(request []byte) (any, error) {
	req, cfg, cred, errPrepare := prepareExecutorRequest(request)
	if errPrepare != nil {
		return nil, errPrepare
	}
	body, errBody := rewriteBody(req.Payload, cfg.upstreamModel(req.Model), nil)
	if errBody != nil {
		return nil, errBody
	}
	ctx, cancel := requestContext(cfg)
	defer cancel()
	resp, errPost := cred.post(ctx, countTokensEndpoint, upstreamHeaders(cred, cfg.ClientVersion, cfg.ClientPlatform, cfg.ClientOSCategory, cfg.ClientOSVersion, ensureDeviceMid(cfg.DeviceMid), req.Headers), body)
	if errPost != nil {
		stats.recordFailure(req.Model, errPost.Error())
		return nil, failf("upstream_unreachable", http.StatusBadGateway, "zcode upstream: %v", errPost)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		stats.recordFailure(req.Model, errRead.Error())
		return nil, failf("upstream_read_failed", http.StatusBadGateway, "read zcode response: %v", errRead)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		stats.recordFailure(req.Model, strings.TrimSpace(string(raw)))
		return nil, upstreamError(resp.StatusCode, raw)
	}
	return executorResponse{Payload: raw, Headers: forwardedResponseHeaders(resp.Header, "application/json")}, nil
}

// executeStream returns as soon as upstream accepted the request and relays the
// remaining SSE events from a background pump, which is what lets the host start
// draining the stream bridge before the first chunk arrives.
func executeStream(request []byte) (any, error) {
	req, cfg, cred, errPrepare := prepareExecutorRequest(request)
	if errPrepare != nil {
		return nil, errPrepare
	}
	if strings.TrimSpace(req.StreamID) == "" {
		return nil, failf("invalid_request", http.StatusInternalServerError, "host did not provide a stream id")
	}
	stream := true
	body, errBody := rewriteBody(req.Payload, cfg.upstreamModel(req.Model), &stream)
	if errBody != nil {
		return nil, errBody
	}
	ctx, cancel := requestContext(cfg)
	start := time.Now()
	resp, errPost := cred.post(ctx, messagesEndpoint, upstreamHeaders(cred, cfg.ClientVersion, cfg.ClientPlatform, cfg.ClientOSCategory, cfg.ClientOSVersion, ensureDeviceMid(cfg.DeviceMid), req.Headers), body)
	if errPost != nil {
		cancel()
		stats.recordFailure(req.Model, errPost.Error())
		return nil, failf("upstream_unreachable", http.StatusBadGateway, "zcode upstream: %v", errPost)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		_ = resp.Body.Close()
		cancel()
		stats.recordFailure(req.Model, strings.TrimSpace(string(raw)))
		return nil, upstreamError(resp.StatusCode, raw)
	}
	stats.observeLatency(time.Since(start))
	streamWG.Add(1)
	trackStream(req.StreamID, cancel)
	go func() {
		defer streamWG.Done()
		defer untrackStream(req.StreamID)
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
		pumpStream(ctx, req.StreamID, resp.Body, req.Model)
	}()
	return executorStreamResponse{Headers: http.Header{"Content-Type": []string{eventStreamMediaType}}}, nil
}
