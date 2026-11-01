package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthMiddleware(t *testing.T) {
	s := &server{cfg: config{APIKey: "secret"}}
	h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("authorized status = %d, want 204", rec.Code)
	}
}

func TestOpenAIProxyAggregatesSSE(t *testing.T) {
	old := chatEndpoint
	defer func() { chatEndpoint = old }()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"model\":\"hy3\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()
	chatEndpoint = upstream.URL + "/v2/chat/completions"
	s := &server{http: upstream.Client()}
	s.auth.Auth.AccessToken = "token"
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"hy3","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	s.openAIChat(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"hello"`) {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
}

func TestAnthropicProxyAggregatesJSON(t *testing.T) {
	old := chatEndpoint
	defer func() { chatEndpoint = old }()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"model\":\"glm-5.2\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()
	chatEndpoint = upstream.URL + "/v2/chat/completions"
	s := &server{http: upstream.Client()}
	s.auth.Auth.AccessToken = "token"
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	s.anthropicMessages(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"type":"message"`) || !strings.Contains(rec.Body.String(), `"hello"`) {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
}
