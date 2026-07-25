// Package gpt 实现 OpenAI 兼容的图像生成 provider（GPT 账号池 → /v1/images/generations）。
//
// 协议：完全对齐 OpenAI Images API，可对接 OpenAI 官方 / Azure / 任意网关。
//
//	POST {base_url}/v1/images/generations
//	Header: Authorization: Bearer {api_key}
//	Body  : {"model","prompt","n","size","response_format"}
//	Resp  : {"created":int,"data":[{"url":"..."} | {"b64_json":"..."}]}
//
// 错误处理：
//   - 4xx 标记账号失败并 30s 冷却（避免雪崩）；
//   - 5xx 标记账号失败并 5min 冷却；
//   - 超时同上。
package gpt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kleinai/backend/internal/provider"
	"github.com/kleinai/backend/pkg/outbound"
)

const (
	defaultBaseURL = "https://api.openai.com"
	defaultTimeout = 6 * time.Minute
)

// Provider 实现 provider.Provider。
type Provider struct {
	client     *http.Client
	defaultURL string
	name       string
}

// New 构造。defaultBase 为空时使用 OpenAI 官方域名。
func New(defaultBase string) *Provider {
	if defaultBase == "" {
		defaultBase = defaultBaseURL
	}
	return &Provider{
		client: &http.Client{
			Timeout: defaultTimeout,
		},
		defaultURL: strings.TrimRight(defaultBase, "/"),
		name:       "gpt",
	}
}

// Name impl。
func (p *Provider) Name() string { return p.name }

type imgReq struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	Quality        string `json:"quality,omitempty"`
	Style          string `json:"style,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
}

type imgRespItem struct {
	URL     string `json:"url"`
	B64JSON string `json:"b64_json,omitempty"`
}
type imgResp struct {
	Created int           `json:"created"`
	Data    []imgRespItem `json:"data"`
	Error   *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

type responseInputItem struct {
	Type     string           `json:"type"`
	Role     string           `json:"role"`
	Content  []map[string]any `json:"content"`
	MetaData map[string]any   `json:"metadata,omitempty"`
}

type responseReq struct {
	Instructions      string           `json:"instructions"`
	Stream            bool             `json:"stream"`
	Reasoning         map[string]any   `json:"reasoning,omitempty"`
	ParallelToolCalls bool             `json:"parallel_tool_calls"`
	Include           []string         `json:"include,omitempty"`
	Model             string           `json:"model"`
	Store             bool             `json:"store"`
	ToolChoice        any              `json:"tool_choice,omitempty"`
	Input             any              `json:"input"`
	Tools             []map[string]any `json:"tools"`
}

type responseCompletedEvent struct {
	Type     string `json:"type"`
	Response struct {
		Output []responseOutputItem `json:"output"`
	} `json:"response"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

type responseOutputItem struct {
	Type          string `json:"type"`
	Result        string `json:"result"`
	B64JSON       string `json:"b64_json"`
	ImageB64      string `json:"image_b64"`
	URL           string `json:"url"`
	OutputFormat  string `json:"output_format"`
	Size          string `json:"size"`
	RevisedPrompt string `json:"revised_prompt"`
	Content       []struct {
		Type     string `json:"type"`
		Result   string `json:"result"`
		B64JSON  string `json:"b64_json"`
		ImageB64 string `json:"image_b64"`
		URL      string `json:"url"`
	} `json:"content"`
}

// Generate impl。仅支持 KindImage。
func (p *Provider) Generate(ctx context.Context, req *provider.Request) (*provider.Result, error) {
	if req.Kind != provider.KindImage {
		return nil, fmt.Errorf("gpt provider only supports image kind, got %s", req.Kind)
	}
	if req.Credential == "" {
		return nil, fmt.Errorf("gpt provider missing credential")
	}
	if isGPTImage2(req.ModelCode) {
		return p.generateImage2(ctx, req)
	}

	base := req.BaseURL
	if base == "" {
		base = p.defaultURL
	}
	base = strings.TrimRight(base, "/")
	url := base + "/v1/images/generations"

	count := req.Count
	if count <= 0 {
		count = 1
	}

	body := imgReq{
		Model:          req.ModelCode,
		Prompt:         req.Prompt,
		N:              count,
		Size:           imageSize(req.Params, "1024x1024"),
		Quality:        strParam(req.Params, "quality", ""),
		Style:          strParam(req.Params, "style", ""),
		ResponseFormat: "url",
	}
	payload, _ := json.Marshal(body)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+req.Credential)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "kleinai/1.0")

	start := time.Now()
	client, err := p.httpClient(req.ProxyURL)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gpt http: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("gpt %d: %s", resp.StatusCode, snippet(raw, 240))
	}

	var out imgResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("gpt decode: %w (raw=%s)", err, snippet(raw, 240))
	}
	if out.Error != nil && out.Error.Message != "" {
		return nil, fmt.Errorf("gpt: %s", out.Error.Message)
	}
	if len(out.Data) == 0 {
		return nil, fmt.Errorf("gpt returned 0 image")
	}

	width, height := parseSize(body.Size)
	assets := make([]provider.Asset, 0, len(out.Data))
	for _, d := range out.Data {
		a := provider.Asset{
			URL:    d.URL,
			Width:  width,
			Height: height,
			Mime:   "image/png",
		}
		if a.URL == "" && d.B64JSON != "" {
			// 大多数网关会直接给 URL；b64 模式 caller 应自行落 OSS 后再回填。
			a.URL = "data:image/png;base64," + d.B64JSON
		}
		assets = append(assets, a)
	}

	return &provider.Result{
		TaskID:  req.TaskID,
		Assets:  assets,
		Latency: time.Since(start),
	}, nil
}

func (p *Provider) generateImage2(ctx context.Context, req *provider.Request) (*provider.Result, error) {
	base := strings.TrimRight(req.BaseURL, "/")
	if base == "" {
		base = p.defaultURL
	}
	url := responseEndpoint(base)
	count := req.Count
	if count <= 0 {
		count = 1
	}
	modelCode := req.ModelCode
	mainModel := strParam(req.Params, "main_model", mainModelForImage2(modelCode))
	toolModel := imageToolModel(modelCode)
	size := imageSize(req.Params, "1024x1024")
	action := "generate"
	if req.Mode == provider.ModeI2I || len(req.RefAssets) > 0 || strings.EqualFold(strParam(req.Params, "operation", ""), "edit") {
		action = "edit"
	}
	content := []map[string]any{{"type": "input_text", "text": req.Prompt}}
	for _, ref := range req.RefAssets {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		content = append(content, map[string]any{"type": "input_image", "image_url": ref})
	}
	input := []responseInputItem{{Type: "message", Role: "user", Content: content}}
	tool := map[string]any{
		"type":   "image_generation",
		"action": action,
		"model":  toolModel,
		"size":   size,
	}
	if quality := imageQuality(req.Params); quality != "" {
		tool["quality"] = quality
	}
	copyParam(tool, req.Params, "background")
	copyParam(tool, req.Params, "output_format")
	copyParam(tool, req.Params, "output_compression")
	copyParam(tool, req.Params, "partial_images")
	copyParam(tool, req.Params, "moderation")
	copyParam(tool, req.Params, "input_fidelity")
	if mask := firstStringParam(req.Params, "mask", "mask_image_url"); mask != "" {
		tool["input_image_mask"] = map[string]string{"image_url": mask}
	}
	body := responseReq{
		Instructions:      "You are an image generation assistant. Follow the user's prompt and return the generated image.",
		Stream:            true,
		Reasoning:         map[string]any{"effort": "medium", "summary": "auto"},
		ParallelToolCalls: true,
		Include:           []string{"reasoning.encrypted_content"},
		Model:             mainModel,
		Store:             false,
		ToolChoice:        "auto",
		Input:             input,
		Tools:             []map[string]any{tool},
	}

	start := time.Now()
	client, err := p.httpClient(req.ProxyURL)
	if err != nil {
		return nil, err
	}
	width, height := parseSize(size)
	assets := make([]provider.Asset, 0, count)
	logUpstream(ctx, req, provider.UpstreamLogEntry{
		Provider: "gpt",
		Stage:    "codex.start",
		Method:   "POST",
		URL:      url,
		Meta: map[string]any{
			"model":          modelCode,
			"main_model":     mainModel,
			"tool_model":     toolModel,
			"size":           size,
			"count":          count,
			"action":         action,
			"ref_count":      len(req.RefAssets),
			"proxy":          req.ProxyURL != "",
			"has_toolchoice": true,
		},
	})
	for i := 0; i < count && len(assets) < count; i++ {
		attemptBody := body
		retriedWithoutToolChoice := false
		for {
			payload, _ := json.Marshal(attemptBody)
			httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			httpReq.Header.Set("Authorization", "Bearer "+req.Credential)
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("Accept", "text/event-stream")
			httpReq.Header.Set("User-Agent", userAgentForEndpoint(url))
			if isCodexEndpoint(url) {
				httpReq.Header.Set("Originator", "codex-tui")
				httpReq.Header.Set("Connection", "Keep-Alive")
			}
			resp, err := client.Do(httpReq)
			if err != nil {
				logUpstream(ctx, req, provider.UpstreamLogEntry{
					Provider:       "gpt",
					Stage:          "codex.request",
					Method:         "POST",
					URL:            url,
					RequestExcerpt: snippet(payload, 600),
					Error:          err.Error(),
					Meta: map[string]any{
						"model":      modelCode,
						"size":       size,
						"count":      count,
						"tool_model": toolModel,
						"action":     action,
					},
				})
				return nil, fmt.Errorf("gpt image2 http: %w", err)
			}
			if resp.StatusCode >= 400 {
				raw, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if retriedWithoutToolChoice {
					logUpstream(ctx, req, provider.UpstreamLogEntry{
						Provider:        "gpt",
						Stage:           "codex.response",
						Method:          "POST",
						URL:             url,
						StatusCode:      resp.StatusCode,
						RequestExcerpt:  snippet(payload, 600),
						ResponseExcerpt: snippet(raw, 600),
						Meta: map[string]any{
							"model":      modelCode,
							"size":       size,
							"count":      count,
							"tool_model": toolModel,
							"action":     action,
						},
					})
				}
				if !retriedWithoutToolChoice && shouldRetryImage2WithoutToolChoice(raw) {
					logUpstream(ctx, req, provider.UpstreamLogEntry{
						Provider:        "gpt",
						Stage:           "codex.retry",
						Method:          "POST",
						URL:             url,
						StatusCode:      resp.StatusCode,
						RequestExcerpt:  snippet(payload, 600),
						ResponseExcerpt: snippet(raw, 600),
						Meta: map[string]any{
							"reason": "tool_choice_fallback",
						},
					})
					attemptBody.ToolChoice = nil
					retriedWithoutToolChoice = true
					continue
				}
				logUpstream(ctx, req, provider.UpstreamLogEntry{
					Provider:        "gpt",
					Stage:           "codex.failed",
					Method:          "POST",
					URL:             url,
					StatusCode:      resp.StatusCode,
					RequestExcerpt:  snippet(payload, 600),
					ResponseExcerpt: snippet(raw, 600),
					Meta: map[string]any{
						"model":      modelCode,
						"size":       size,
						"count":      count,
						"tool_model": toolModel,
						"action":     action,
					},
				})
				return nil, fmt.Errorf("gpt image2 %d: %s", resp.StatusCode, snippet(raw, 320))
			}
			completed, err := parseCompletedResponse(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				logUpstream(ctx, req, provider.UpstreamLogEntry{
					Provider:       "gpt",
					Stage:          "codex.decode",
					Method:         "POST",
					URL:            url,
					RequestExcerpt: snippet(payload, 600),
					Error:          err.Error(),
					Meta: map[string]any{
						"model":      modelCode,
						"size":       size,
						"count":      count,
						"tool_model": toolModel,
						"action":     action,
					},
				})
				return nil, err
			}
			if completed.Error != nil && completed.Error.Message != "" {
				logUpstream(ctx, req, provider.UpstreamLogEntry{
					Provider:        "gpt",
					Stage:           "codex.failed",
					Method:          "POST",
					URL:             url,
					RequestExcerpt:  snippet(payload, 600),
					ResponseExcerpt: completed.Error.Message,
					Meta: map[string]any{
						"model":      modelCode,
						"size":       size,
						"count":      count,
						"tool_model": toolModel,
						"action":     action,
					},
				})
				return nil, fmt.Errorf("gpt image2: %s", completed.Error.Message)
			}
			for _, out := range completed.Response.Output {
				imageData, imageURL := outputImagePayload(out)
				if out.Type != "image_generation_call" && imageData == "" && imageURL == "" {
					continue
				}
				mime := mimeForImageFormat(out.OutputFormat)
				assetWidth, assetHeight := width, height
				if out.Size != "" {
					assetWidth, assetHeight = parseSize(out.Size)
				}
				assetURL := imageURL
				if assetURL == "" {
					assetURL = "data:" + mime + ";base64," + imageData
				}
				assets = append(assets, provider.Asset{
					URL:    assetURL,
					Width:  assetWidth,
					Height: assetHeight,
					Mime:   mime,
					Meta:   map[string]any{"revised_prompt": out.RevisedPrompt, "provider_action": action, "size": size},
				})
				logUpstream(ctx, req, provider.UpstreamLogEntry{
					Provider:        "gpt",
					Stage:           "codex.asset",
					Method:          "POST",
					URL:             url,
					RequestExcerpt:  snippet(payload, 600),
					ResponseExcerpt: assetURL,
					Meta: map[string]any{
						"model":       modelCode,
						"size":        size,
						"count":       count,
						"tool_model":  toolModel,
						"action":      action,
						"asset_index": len(assets),
					},
				})
				if len(assets) >= count {
					break
				}
			}
			break
		}
	}
	if len(assets) == 0 {
		logUpstream(ctx, req, provider.UpstreamLogEntry{
			Provider:        "gpt",
			Stage:           "codex.failed",
			Method:          "POST",
			URL:             url,
			ResponseExcerpt: "gpt image2 returned 0 image",
			Meta: map[string]any{
				"model":      modelCode,
				"size":       size,
				"count":      count,
				"tool_model": toolModel,
				"action":     action,
			},
		})
		return nil, fmt.Errorf("gpt image2 returned 0 image")
	}
	logUpstream(ctx, req, provider.UpstreamLogEntry{
		Provider: "gpt",
		Stage:    "codex.success",
		Method:   "POST",
		URL:      url,
		Meta: map[string]any{
			"model":      modelCode,
			"size":       size,
			"count":      count,
			"tool_model": toolModel,
			"action":     action,
			"assets":     len(assets),
		},
	})
	return &provider.Result{TaskID: req.TaskID, Assets: assets, Latency: time.Since(start)}, nil
}

// === helpers ===

func strParam(p map[string]any, key, def string) string {
	if p == nil {
		return def
	}
	if v, ok := p[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return def
}

func (p *Provider) httpClient(proxyURL string) (*http.Client, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return p.client, nil
	}
	return outbound.NewClient(outbound.Options{
		ProxyURL: proxyURL,
		Timeout:  defaultTimeout,
		Mode:     outbound.ModeUTLS,
		Profile:  outbound.ProfileChrome,
	})
}

func firstStringParam(p map[string]any, keys ...string) string {
	for _, key := range keys {
		if v := strParam(p, key, ""); v != "" {
			return v
		}
	}
	return ""
}

func copyParam(dst map[string]any, src map[string]any, key string) {
	if src == nil {
		return
	}
	if v, ok := src[key]; ok {
		switch t := v.(type) {
		case string:
			if t != "" {
				dst[key] = t
			}
		default:
			dst[key] = v
		}
	}
}

func isGPTImage2(model string) bool {
	return imageToolModel(model) == "gpt-image-2"
}

func imageToolModel(model string) string {
	model = strings.TrimSpace(model)
	if idx := strings.LastIndex(model, "/"); idx >= 0 {
		model = model[idx+1:]
	}
	return model
}

func shouldRetryImage2WithoutToolChoice(raw []byte) bool {
	msg := strings.ToLower(string(raw))
	return strings.Contains(msg, "tool choice") &&
		strings.Contains(msg, "image_generation") &&
		strings.Contains(msg, "not found") &&
		strings.Contains(msg, "tools")
}

func mainModelForImage2(model string) string {
	model = strings.TrimSpace(model)
	if idx := strings.LastIndex(model, "/"); idx > 0 {
		return model[:idx] + "/gpt-5.5"
	}
	return "gpt-5.5"
}

func responseEndpoint(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, "/responses") {
		return base
	}
	if strings.Contains(base, "/backend-api/codex") {
		return base + "/responses"
	}
	return base + "/v1/responses"
}

func isCodexBase(base string) bool {
	return strings.Contains(strings.ToLower(base), "/backend-api/codex")
}

func isCodexEndpoint(url string) bool {
	return strings.Contains(strings.ToLower(url), "chatgpt.com/backend-api/codex")
}

func userAgentForEndpoint(url string) string {
	if isCodexEndpoint(url) {
		return "codex-tui/0.118.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.118.0)"
	}
	return "kleinai/1.0"
}

func imageSize(params map[string]any, def string) string {
	if size := strParam(params, "size", ""); size != "" {
		return size
	}
	ratio := strParam(params, "ratio", strParam(params, "aspect_ratio", "1:1"))
	tier := strings.ToUpper(strParam(params, "resolution", strParam(params, "size_tier", "1K")))
	sizes := map[string]map[string]string{
		"1K": {
			"1:1":  "1024x1024",
			"3:2":  "1216x832",
			"2:3":  "832x1216",
			"4:3":  "1152x864",
			"3:4":  "864x1152",
			"5:4":  "1120x896",
			"4:5":  "896x1120",
			"16:9": "1344x768",
			"9:16": "768x1344",
			"21:9": "1536x640",
		},
		"2K": {
			"1:1":  "1248x1248",
			"3:2":  "1536x1024",
			"2:3":  "1024x1536",
			"4:3":  "1440x1088",
			"3:4":  "1088x1440",
			"5:4":  "1392x1120",
			"4:5":  "1120x1392",
			"16:9": "1664x928",
			"9:16": "928x1664",
			"21:9": "1904x816",
		},
		"4K": {
			"1:1":  "2480x2480",
			"3:2":  "3056x2032",
			"2:3":  "2032x3056",
			"4:3":  "2880x2160",
			"3:4":  "2160x2880",
			"5:4":  "2784x2224",
			"4:5":  "2224x2784",
			"16:9": "3312x1872",
			"9:16": "1872x3312",
			"21:9": "3808x1632",
		},
	}
	if byRatio, ok := sizes[tier]; ok {
		if size := byRatio[ratio]; size != "" {
			return size
		}
		return byRatio["1:1"]
	}
	if byRatio := sizes["1K"]; byRatio != nil {
		if size := byRatio[ratio]; size != "" {
			return size
		}
	}
	return def
}

func imageQuality(params map[string]any) string {
	switch strings.ToLower(strParam(params, "quality", "")) {
	case "draft", "low":
		return "low"
	case "standard", "medium":
		return "medium"
	case "hd", "high":
		return "high"
	default:
		return ""
	}
}

func logUpstream(ctx context.Context, req *provider.Request, entry provider.UpstreamLogEntry) {
	if req == nil || req.UpstreamLog == nil {
		return
	}
	if entry.Provider == "" {
		entry.Provider = "gpt"
	}
	req.UpstreamLog(ctx, entry)
}

func outputImagePayload(out responseOutputItem) (string, string) {
	if out.Result != "" {
		return out.Result, ""
	}
	if out.B64JSON != "" {
		return out.B64JSON, ""
	}
	if out.ImageB64 != "" {
		return out.ImageB64, ""
	}
	if out.URL != "" {
		return "", out.URL
	}
	for _, content := range out.Content {
		if content.Result != "" {
			return content.Result, ""
		}
		if content.B64JSON != "" {
			return content.B64JSON, ""
		}
		if content.ImageB64 != "" {
			return content.ImageB64, ""
		}
		if content.URL != "" {
			return "", content.URL
		}
	}
	return "", ""
}

func parseCompletedResponse(r io.Reader) (*responseCompletedEvent, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var dataLines []string
	var last *responseCompletedEvent
	var outputItems []responseOutputItem
	var partialItems []responseOutputItem
	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = nil
		if data == "" || data == "[DONE]" {
			return nil
		}
		var ev responseCompletedEvent
		err := json.Unmarshal([]byte(data), &ev)
		var direct struct {
			Output []responseOutputItem `json:"output"`
			Item   responseOutputItem   `json:"item"`
		}
		if err2 := json.Unmarshal([]byte(data), &direct); err2 == nil {
			if len(ev.Response.Output) == 0 && len(direct.Output) > 0 {
				ev.Type = "response.completed"
				ev.Response.Output = direct.Output
			}
			if direct.Item.Type != "" && ev.Type == "" {
				ev.Type = "response.output_item.done"
			}
		}
		if err != nil && len(ev.Response.Output) == 0 && direct.Item.Type == "" {
			return err
		}
		switch ev.Type {
		case "response.output_item.done":
			if direct.Item.Type != "" {
				outputItems = append(outputItems, direct.Item)
			}
		case "response.image_generation_call.partial_image":
			var partial struct {
				OutputFormat string `json:"output_format"`
				PartialB64   string `json:"partial_image_b64"`
			}
			if err := json.Unmarshal([]byte(data), &partial); err == nil && partial.PartialB64 != "" {
				partialItems = append(partialItems, responseOutputItem{
					Type:         "image_generation_call",
					Result:       partial.PartialB64,
					OutputFormat: partial.OutputFormat,
				})
			}
		}
		if ev.Type == "response.completed" || len(ev.Response.Output) > 0 || ev.Error != nil {
			last = &ev
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return nil, fmt.Errorf("gpt image2 stream decode: %w", err)
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("gpt image2 stream read: %w", err)
	}
	if err := flush(); err != nil {
		return nil, fmt.Errorf("gpt image2 stream decode: %w", err)
	}
	if last == nil {
		last = &responseCompletedEvent{Type: "response.completed"}
	}
	if len(last.Response.Output) == 0 && len(outputItems) > 0 {
		last.Response.Output = outputItems
	}
	if len(last.Response.Output) == 0 && len(partialItems) > 0 {
		last.Response.Output = partialItems
	}
	return last, nil
}

func mimeForImageFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "jpeg", "jpg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func parseSize(size string) (int, int) {
	if size == "" {
		return 1024, 1024
	}
	parts := strings.SplitN(size, "x", 2)
	if len(parts) != 2 {
		return 1024, 1024
	}
	var w, h int
	fmt.Sscanf(parts[0], "%d", &w)
	fmt.Sscanf(parts[1], "%d", &h)
	if w <= 0 {
		w = 1024
	}
	if h <= 0 {
		h = 1024
	}
	return w, h
}

func snippet(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	r := []rune(string(b))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "...(truncated)"
}
