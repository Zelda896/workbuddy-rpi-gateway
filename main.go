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
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultListen = "127.0.0.1:8080"

var (
	upstreamBase = "https://copilot.tencent.com"
	chatEndpoint = "https://copilot.tencent.com/v2/chat/completions"
	refreshURL   = "https://copilot.tencent.com/v2/plugin/auth/token/refresh"
)

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type storedAuth struct {
	Auth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
	} `json:"auth"`
	Account struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		TenantID     string `json:"tenantId"`
		Nickname     string `json:"nickname"`
	} `json:"account"`
}

type config struct {
	Listen       string
	APIKey       string
	AuthFile     string
	AccessToken  string
	RefreshToken string
	UserID       string
	EnterpriseID string
	TenantID     string
	Domain       string
}

type server struct {
	cfg  config
	mu   sync.RWMutex
	auth storedAuth
	http *http.Client
}

func main() {
	cfg := loadConfig()
	if len(os.Args) > 1 && os.Args[1] == "login" {
		if err := login(cfg); err != nil {
			log.Fatal(err)
		}
		return
	}
	s := &server{cfg: cfg, http: &http.Client{Timeout: 0}}
	if err := s.loadAuth(); err != nil {
		log.Printf("auth not loaded: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/v1/models", s.models)
	mux.HandleFunc("/v1/chat/completions", s.openAIChat)
	mux.HandleFunc("/v1/messages", s.anthropicMessages)
	mux.HandleFunc("/v1/messages/count_tokens", s.countTokens)
	log.Printf("workbuddy gateway listening on %s", cfg.Listen)
	if err := http.ListenAndServe(cfg.Listen, s.authMiddleware(mux)); err != nil {
		log.Fatal(err)
	}
}

func login(cfg config) error {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(http.MethodPost, upstreamBase+"/v2/plugin/auth/state?platform=CLI", strings.NewReader("{}"))
	if err != nil {
		return err
	}
	setCommonHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request login state: %w", err)
	}
	defer resp.Body.Close()
	var env apiEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("decode login state: %w", err)
	}
	var state struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if env.Code != 0 || json.Unmarshal(env.Data, &state) != nil || state.State == "" || state.AuthURL == "" {
		return fmt.Errorf("login state rejected: %s", env.Msg)
	}
	fmt.Println("请在任意电脑或手机浏览器打开以下地址完成 WorkBuddy 登录：")
	fmt.Println(state.AuthURL)
	fmt.Println("登录完成后回到此处，程序会自动保存凭据。")
	for i := 0; i < 120; i++ {
		time.Sleep(3 * time.Second)
		poll, err := http.NewRequest(http.MethodGet, upstreamBase+"/v2/plugin/auth/token?state="+url.QueryEscape(state.State), nil)
		if err != nil {
			return err
		}
		setCommonHeaders(poll)
		pollResp, err := client.Do(poll)
		if err != nil {
			continue
		}
		var tokenEnv apiEnvelope
		decodeErr := json.NewDecoder(pollResp.Body).Decode(&tokenEnv)
		pollResp.Body.Close()
		if decodeErr != nil || tokenEnv.Code != 0 {
			fmt.Print(".")
			continue
		}
		var token struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			Domain       string `json:"domain"`
		}
		if json.Unmarshal(tokenEnv.Data, &token) != nil || token.AccessToken == "" {
			fmt.Print(".")
			continue
		}
		auth := storedAuth{}
		auth.Auth.AccessToken = token.AccessToken
		auth.Auth.RefreshToken = token.RefreshToken
		auth.Auth.Domain = token.Domain
		auth.Auth.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).Unix()
		if cfg.AuthFile == "" {
			return errors.New("WORKBUDDY_AUTH_FILE must be set for login")
		}
		raw, _ := json.MarshalIndent(auth, "", "  ")
		if err := os.WriteFile(cfg.AuthFile, raw, 0600); err != nil {
			return fmt.Errorf("save auth file: %w", err)
		}
		fmt.Printf("\n登录成功，凭据已保存到 %s\n", cfg.AuthFile)
		return nil
	}
	return errors.New("login timed out after 6 minutes")
}

func setCommonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", "WorkBuddy/5.5.6 CLI/2.63.2")
	req.Header.Set("Origin", "https://www.codebuddy.cn")
	req.Header.Set("Referer", "https://www.codebuddy.cn/")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
}

func loadConfig() config {
	listen := os.Getenv("WORKBUDDY_LISTEN")
	if listen == "" {
		listen = defaultListen
	}
	return config{
		Listen: listen, APIKey: os.Getenv("WORKBUDDY_API_KEY"),
		AuthFile:     os.Getenv("WORKBUDDY_AUTH_FILE"),
		AccessToken:  os.Getenv("WORKBUDDY_ACCESS_TOKEN"),
		RefreshToken: os.Getenv("WORKBUDDY_REFRESH_TOKEN"),
		UserID:       os.Getenv("WORKBUDDY_USER_ID"),
		EnterpriseID: os.Getenv("WORKBUDDY_ENTERPRISE_ID"),
		TenantID:     os.Getenv("WORKBUDDY_TENANT_ID"),
		Domain:       os.Getenv("WORKBUDDY_DOMAIN"),
	}
}

func (s *server) loadAuth() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.AuthFile != "" {
		raw, err := os.ReadFile(s.cfg.AuthFile)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &s.auth); err != nil {
			return fmt.Errorf("parse auth file: %w", err)
		}
	}
	if s.cfg.AccessToken != "" {
		s.auth.Auth.AccessToken = s.cfg.AccessToken
	}
	if s.cfg.RefreshToken != "" {
		s.auth.Auth.RefreshToken = s.cfg.RefreshToken
	}
	if s.cfg.UserID != "" {
		s.auth.Account.UID = s.cfg.UserID
	}
	if s.cfg.EnterpriseID != "" {
		s.auth.Account.EnterpriseID = s.cfg.EnterpriseID
	}
	if s.cfg.TenantID != "" {
		s.auth.Account.TenantID = s.cfg.TenantID
	}
	if s.cfg.Domain != "" {
		s.auth.Auth.Domain = s.cfg.Domain
	}
	if s.auth.Auth.AccessToken == "" {
		return errors.New("no access token configured")
	}
	return nil
}

func (s *server) saveAuth() error {
	if s.cfg.AuthFile == "" {
		return nil
	}
	s.mu.RLock()
	raw, err := json.MarshalIndent(s.auth, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	return os.WriteFile(s.cfg.AuthFile, raw, 0600)
}

func (s *server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if key := s.cfg.APIKey; key != "" && !validAPIKey(r, key) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"message": "invalid API key", "type": "authentication_error", "code": "invalid_api_key",
			}})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func validAPIKey(r *http.Request, expected string) bool {
	if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == expected {
		return true
	}
	return r.Header.Get("x-api-key") == expected
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	ready := s.auth.Auth.AccessToken != ""
	s.mu.RUnlock()
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ok": ready})
}

func (s *server) models(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().Unix()
	models := []string{"auto", "default-1.1", "default-1.2", "deepseek-v4-pro", "deepseek-v4-flash",
		"deepseek-v3-2-volc", "minimax-m2.5", "minimax-m3", "minimax-m2.7", "glm-5.2", "glm-5.1",
		"glm-5.0", "glm-5v-turbo", "kimi-k3-1", "kimi-k2.7", "hy3", "hy3-preview",
		"hunyuan-2.0-thinking"}
	data := make([]map[string]any, 0, len(models))
	for _, id := range models {
		data = append(data, map[string]any{"id": id, "object": "model", "created": now, "owned_by": "workbuddy"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *server) openAIChat(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	stream, _ := body["stream"].(bool)
	body["stream"] = true
	if model, _ := body["model"].(string); strings.HasPrefix(model, "hy3") {
		body["reasoning_effort"] = "high"
	}
	if err := s.proxy(w, r, body, stream, protocolOpenAI); err != nil {
		log.Printf("chat request failed: %v", err)
	}
}

func (s *server) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	var in anthropicRequest
	if err := decodeBody(r, &in); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	body := map[string]any{"model": in.Model, "stream": true, "max_tokens": in.MaxTokens}
	messages := make([]any, 0, len(in.Messages)+1)
	if in.System != nil {
		messages = append(messages, map[string]any{"role": "system", "content": in.System})
	}
	for _, m := range in.Messages {
		messages = append(messages, map[string]any{"role": m.Role, "content": m.Content})
	}
	body["messages"] = messages
	if in.Temperature != nil {
		body["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		body["top_p"] = *in.TopP
	}
	if len(in.Tools) > 0 {
		body["tools"] = in.Tools
	}
	if err := s.proxy(w, r, body, in.Stream, protocolAnthropic); err != nil {
		log.Printf("messages request failed: %v", err)
	}
}

func (s *server) countTokens(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Conservative estimate; the upstream does not expose a tokenizer endpoint.
	total := 0
	for _, m := range in.Messages {
		b, _ := json.Marshal(m.Content)
		total += (len(b) + 3) / 4
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": total})
}

type protocol int

const (
	protocolOpenAI protocol = iota
	protocolAnthropic
)

func (s *server) proxy(w http.ResponseWriter, r *http.Request, body map[string]any, stream bool, p protocol) error {
	raw, err := json.Marshal(body)
	if err != nil {
		writeProtocolError(w, http.StatusBadRequest, p, err.Error())
		return err
	}
	resp, err := s.doUpstream(r.Context(), raw)
	if err != nil {
		writeProtocolError(w, http.StatusBadGateway, p, err.Error())
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		if err := s.refresh(); err == nil {
			resp.Body.Close()
			resp, err = s.doUpstream(r.Context(), raw)
			if err != nil {
				writeProtocolError(w, http.StatusBadGateway, p, err.Error())
				return err
			}
			defer resp.Body.Close()
		}
	}
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		writeProtocolError(w, resp.StatusCode, p, strings.TrimSpace(string(data)))
		return fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	if stream {
		return s.streamResponse(w, resp.Body, p)
	}
	return s.aggregateResponse(w, resp.Body, p)
}

func (s *server) doUpstream(ctx context.Context, raw []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatEndpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	s.setHeaders(req)
	return s.http.Do(req)
}

func (s *server) setHeaders(req *http.Request) {
	s.mu.RLock()
	auth := s.auth
	s.mu.RUnlock()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "WorkBuddy/5.5.6 CLI/2.63.2")
	req.Header.Set("Origin", "https://www.codebuddy.cn")
	req.Header.Set("Referer", "https://www.codebuddy.cn/")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-Product-Version", "5.5.6")
	req.Header.Set("X-Agent-Type", "main")
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-Private-Data", "true")
	req.Header.Set("Authorization", "Bearer "+auth.Auth.AccessToken)
	if auth.Auth.RefreshToken != "" {
		req.Header.Set("X-Refresh-Token", auth.Auth.RefreshToken)
	}
	if auth.Auth.Domain != "" {
		req.Header.Set("X-Domain", auth.Auth.Domain)
	}
	if auth.Account.UID != "" {
		req.Header.Set("X-User-Id", auth.Account.UID)
	}
	if auth.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", auth.Account.EnterpriseID)
	}
	if auth.Account.TenantID != "" {
		req.Header.Set("X-Tenant-Id", auth.Account.TenantID)
	}
	for _, name := range []string{"X-Request-ID", "X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID"} {
		req.Header.Set(name, newID())
	}
}

func (s *server) refresh() error {
	s.mu.RLock()
	refresh := s.auth.Auth.RefreshToken
	enterprise := s.auth.Account.EnterpriseID
	s.mu.RUnlock()
	if refresh == "" {
		return errors.New("no refresh token")
	}
	req, err := http.NewRequest(http.MethodPost, refreshURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Refresh-Token", refresh)
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
	if enterprise != "" {
		req.Header.Set("X-Enterprise-Id", enterprise)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			Domain       string `json:"domain"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return err
	}
	if resp.StatusCode >= 400 || env.Code != 0 || env.Data.AccessToken == "" {
		return fmt.Errorf("refresh rejected: %s", env.Msg)
	}
	s.mu.Lock()
	s.auth.Auth.AccessToken = env.Data.AccessToken
	if env.Data.RefreshToken != "" {
		s.auth.Auth.RefreshToken = env.Data.RefreshToken
	}
	if env.Data.Domain != "" {
		s.auth.Auth.Domain = env.Data.Domain
	}
	if env.Data.ExpiresIn > 0 {
		s.auth.Auth.ExpiresAt = time.Now().Add(time.Duration(env.Data.ExpiresIn) * time.Second).Unix()
	}
	s.mu.Unlock()
	return s.saveAuth()
}

func (s *server) streamResponse(w http.ResponseWriter, body io.Reader, p protocol) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	anthropicStarted := false
	for sc.Scan() {
		line := sc.Text()
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		if p == protocolOpenAI {
			fmt.Fprintf(w, "data: %s\n\n", data)
		} else {
			var chunk map[string]any
			if json.Unmarshal([]byte(data), &chunk) == nil {
				if !anthropicStarted {
					id, model := stringField(chunk, "id"), stringField(chunk, "model")
					fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", jsonString(map[string]any{
						"type":    "message_start",
						"message": map[string]any{"id": id, "type": "message", "role": "assistant", "content": []any{}, "model": model, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}},
					}))
					fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
					anthropicStarted = true
				}
				if out := anthropicEvent(data); out != "" {
					fmt.Fprint(w, out)
				}
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if p == protocolOpenAI {
		fmt.Fprint(w, "data: [DONE]\n\n")
	} else if anthropicStarted {
		fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":0}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

func (s *server) aggregateResponse(w http.ResponseWriter, body io.Reader, p protocol) error {
	var content, reasoning string
	var last map[string]any
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		data := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		last = chunk
		if choices, ok := chunk["choices"].([]any); ok && len(choices) > 0 {
			if c, ok := choices[0].(map[string]any); ok {
				if d, ok := c["delta"].(map[string]any); ok {
					content += stringField(d, "content")
					reasoning += stringField(d, "reasoning_content")
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if p == protocolOpenAI {
		choice := map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}
		if reasoning != "" {
			choice["message"].(map[string]any)["reasoning_content"] = reasoning
		}
		out := map[string]any{"id": last["id"], "object": "chat.completion", "created": last["created"], "model": last["model"], "choices": []any{choice}}
		writeJSON(w, http.StatusOK, out)
	} else {
		id := stringField(last, "id")
		model := stringField(last, "model")
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": model,
			"content":     []any{map[string]any{"type": "text", "text": content}},
			"stop_reason": "end_turn", "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		})
	}
	return nil
}

func anthropicEvent(data string) string {
	var c map[string]any
	if json.Unmarshal([]byte(data), &c) != nil {
		return ""
	}
	id, model := stringField(c, "id"), stringField(c, "model")
	choices, _ := c["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	content := stringField(delta, "content")
	if content == "" {
		return ""
	}
	return "event: content_block_delta\ndata: " + jsonString(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": content}, "id": id, "model": model}) + "\n\n"
}

type anthropicRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
	System      any      `json:"system"`
	MaxTokens   int      `json:"max_tokens"`
	Stream      bool     `json:"stream"`
	Temperature *float64 `json:"temperature"`
	TopP        *float64 `json:"top_p"`
	Tools       []any    `json:"tools"`
}

func decodeBody(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 32<<20))
	return dec.Decode(v)
}
func stringField(m map[string]any, key string) string { v, _ := m[key].(string); return v }
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b[:])
}
func jsonString(v any) string { b, _ := json.Marshal(v); return string(b) }
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeOpenAIError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": typ}})
}
func writeAnthropicError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": msg}})
}
func writeProtocolError(w http.ResponseWriter, status int, p protocol, msg string) {
	if p == protocolAnthropic {
		writeAnthropicError(w, status, msg)
	} else {
		writeOpenAIError(w, status, "upstream_error", msg)
	}
}
