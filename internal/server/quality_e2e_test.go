package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hm2899/grokcli-2api/internal/pool"
	"github.com/hm2899/grokcli-2api/internal/proxy"
	"github.com/hm2899/grokcli-2api/internal/upstream/grok"
)

type mockMultiAccountRoundTripper struct {
	round int
}

func (m *mockMultiAccountRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	m.round++
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream")
	rec.WriteHeader(http.StatusOK)

	auth := req.Header.Get("Authorization")
	if strings.Contains(auth, "acc-degraded") {
		// Degraded stream: claims reasoning in usage but no thinking delta / stub only
		_, _ = rec.WriteString(sseLines(
			": grok2api-reasoning-start",
			`{"type":"response.output_text.delta","delta":"Here is a direct answer without thinking step."}`,
			`{"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":25,"output_tokens_details":{"reasoning_tokens":0}}}}`,
		))
	} else {
		// Healthy stream: real streamed thinking delta
		_, _ = rec.WriteString(sseLines(
			`{"type":"response.reasoning_text.delta","delta":"Let me think carefully about this question."}`,
			`{"type":"response.output_text.delta","delta":"Here is the verified answer."}`,
			`{"type":"response.completed","response":{"id":"resp_2","usage":{"output_tokens":100,"output_tokens_details":{"reasoning_tokens":40}}}}`,
		))
	}
	return rec.Result(), nil
}

func TestQualityDegradedAutoFailoverE2E(t *testing.T) {
	mockRT := &mockMultiAccountRoundTripper{}
	mockClient := &http.Client{Transport: mockRT}

	grokClient := &grok.Client{
		BaseURL: "http://upstream.mock/v1",
		HTTP:    mockClient,
	}

	service := proxy.ChatService{
		Client: grokClient,
	}

	candidates := []pool.Candidate{
		{ID: "acc-1", Token: "acc-degraded-token", Enabled: true, Weight: 1},
		{ID: "acc-2", Token: "acc-healthy-token", Enabled: true, Weight: 1},
	}

	qualityCfg := DefaultQualityRetryRuntime()
	chatReq := proxy.ChatRequest{
		Model:  "grok-4.5",
		Stream: true,
		Raw:    map[string]any{"model": "grok-4.5", "stream": true},
	}

	exclude := map[string]struct{}{}
	candPool := candidates
	var chosenAccount string
	var finalStream io.ReadCloser
	var finalVerdict QualityVerdict

	for attempt := 0; attempt < qualityCfg.MaxAttempts; attempt++ {
		filtered := make([]pool.Candidate, 0, len(candPool))
		for _, c := range candPool {
			if _, bad := exclude[c.ID]; !bad {
				filtered = append(filtered, c)
			}
		}
		if len(filtered) == 0 {
			t.Fatal("exhausted candidates without healthy stream")
		}

		opened, err := service.OpenStreamWithResult(context.Background(), chatReq, filtered, "round_robin")
		if err != nil {
			t.Fatalf("OpenStream failed: %v", err)
		}

		replayed, verdict, _, _, peekErr := peekQualityStream(context.Background(), opened.Body, qualityProtocolResponses, qualityCfg)
		if peekErr != nil || verdict == QualityWithhold {
			_ = opened.Body.Close()
			exclude[opened.AccountID] = struct{}{}
			continue
		}

		chosenAccount = opened.AccountID
		finalStream = replayed
		finalVerdict = verdict
		break
	}

	if chosenAccount != "acc-2" {
		t.Fatalf("expected healthy acc-2 to be chosen after acc-1 degraded, got %s", chosenAccount)
	}
	if finalVerdict != QualityDeliver {
		t.Fatalf("expected QualityDeliver, got %s", finalVerdict)
	}

	bodyBytes, err := io.ReadAll(finalStream)
	if err != nil {
		t.Fatalf("read final stream: %v", err)
	}
	if !strings.Contains(string(bodyBytes), "Let me think carefully") {
		t.Fatalf("replayed stream missing thinking content: %s", string(bodyBytes))
	}
}

func TestMuxQualityHoldRotatesDegradedStreamToHealthyAccount(t *testing.T) {
	mockRT := &mockMultiAccountRoundTripper{}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RequestURI = ""
		resp, err := mockRT.RoundTrip(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer upstream.Close()

	opts := Options{
		Ready:            func() bool { return true },
		ChatEnabled:      true,
		ResponsesEnabled: true,
		MessagesEnabled:  true,
		Candidates: []pool.Candidate{
			{ID: "acc-1", Token: "acc-degraded-token", Enabled: true, Weight: 1},
			{ID: "acc-2", Token: "acc-healthy-token", Enabled: true, Weight: 1},
		},
	}
	opts.Config.UpstreamBase = upstream.URL + "/v1"
	opts.Config.DefaultModel = "grok-4.5"
	opts.Config.SSEKeepalive = 4

	h := NewMux(opts)

	t.Run("openai chat stream", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4.5","stream":true,"messages":[{"role":"user","content":"explain quantum physics"}]}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected HTTP 200, got status %d body %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(body, "direct answer without thinking step") {
			t.Fatalf("degraded output was leaked to client: %s", body)
		}
		if !strings.Contains(body, "verified answer") && !strings.Contains(body, "Let me think carefully") {
			t.Fatalf("healthy answer missing from client response: %s", body)
		}
	})

	t.Run("openai responses stream", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-4.5","stream":true,"input":[{"role":"user","content":"explain quantum physics"}]}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected HTTP 200, got status %d body %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(body, "direct answer without thinking step") {
			t.Fatalf("degraded output was leaked to client: %s", body)
		}
		if !strings.Contains(body, "verified answer") {
			t.Fatalf("healthy answer missing from responses client: %s", body)
		}
	})

	t.Run("anthropic messages stream", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"grok-4.5","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"explain quantum physics"}]}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected HTTP 200, got status %d body %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(body, "direct answer without thinking step") {
			t.Fatalf("degraded output was leaked to anthropic client: %s", body)
		}
		if !strings.Contains(body, "verified answer") {
			t.Fatalf("healthy answer missing from anthropic client: %s", body)
		}
	})
}
