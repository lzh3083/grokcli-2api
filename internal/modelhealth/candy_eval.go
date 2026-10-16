package modelhealth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hm2899/grokcli-2api/internal/store/postgres"
	"github.com/hm2899/grokcli-2api/internal/upstream/grok"
)

// CandyProblemPrompt 标准糖果测试题（正解必定为 21）。
const CandyProblemPrompt = `不使用任何外部工具回答以下问题：

在一个黑色的袋子里放有三种口味的糖果，每种糖果有两种不同的形状（圆形和五角星形，不同的形状靠手感可以分辨）。现已知不同口味的糖和不同形状的数量统计如下表。参赛者需要在活动前决定摸出的糖果数目，那么，最少取出多少个糖果才能保证手中同时拥有不同形状的苹果味和桃子味的糖？（同时手中有圆形苹果味匹配五角星桃子味糖果，或者有圆形桃子味匹配五角星苹果味糖果都满足要求）

        苹果味  桃子味  西瓜味
圆形       7      9      8
五角星形   7      6      4

请逐步推理分析，并在回答的最后一行仅写出最终的整数数字，不要有其他文字。`

// 判定结果枚举
const (
	CandyVerdictPass             = "candy_pass"               // 答案正确(21)且思考正常
	CandyVerdictPassLowThinking  = "candy_pass_low_reasoning"  // 答出21但推理token偏少
	CandyVerdictDegraded         = "candy_degraded"           // 答错(未得出21)，确诊降智
	CandyVerdictDegradedTPSSpike = "candy_degraded_tps_spike" // 答案错误且TPS异常飙升(疑似浅层模型替换)
	CandyVerdictEmpty            = "candy_empty"              // 空输出
	CandyVerdictError            = "candy_error"              // 上游报错/网络异常
)

var (
	reFinalLineNum = regexp.MustCompile(`(?m)^\s*(\d{1,4})\s*$`)
	reBoldNum      = regexp.MustCompile(`\*\*(\d{1,4})\*\*`)
	reAnswerPrefix = regexp.MustCompile(`(?:最少(?:需要|需)?(?:取出)?|答案[为是：:]|总共(?:需要|需)?(?:取出)?|至少(?:需要|需)?(?:取出)?|需要取出|必须取出|即为|结[论果][为是：:]|因此[为是：:]?)[^\d\n]*?(\d{1,4})`)
)

// CandyEvalResult 糖果测试结果
type CandyEvalResult struct {
	Passed           bool    `json:"passed"`
	Verdict          string  `json:"verdict"`
	ExtractedAnswer  int     `json:"extracted_answer"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TPS              float64 `json:"tps"`
	ContentSnippet   string  `json:"content_snippet"`
	Summary          string  `json:"summary"`
}

// ExtractCandyAnswer 从模型输出正文中提取最终给出的整数答案。
func ExtractCandyAnswer(content string) (int, bool) {
	clean := strings.TrimSpace(content)
	if clean == "" {
		return 0, false
	}

	// 1. 优先提取倒数非空行的独立数字（按照 Prompt 要求："在回答的最后一行仅写出最终的整数数字"）
	lines := strings.Split(clean, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		// 尝试剥离常见的句尾标点
		trimmedLine := strings.TrimRight(line, "。.,!！个颗")
		trimmedLine = strings.TrimSpace(trimmedLine)
		if n, err := strconv.Atoi(trimmedLine); err == nil && n > 0 && n < 10000 {
			return n, true
		}
		if m := reFinalLineNum.FindStringSubmatch(line); len(m) > 1 {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return n, true
			}
		}
		// 只看最后 3 个非空行，避免看太前
		if len(lines)-i > 3 {
			break
		}
	}

	// 2. 匹配加粗结论，例如 **21**
	if matches := reBoldNum.FindAllStringSubmatch(clean, -1); len(matches) > 0 {
		lastMatch := matches[len(matches)-1]
		if n, err := strconv.Atoi(lastMatch[1]); err == nil {
			return n, true
		}
	}

	// 3. 匹配常见结论句式："至少需要 36 颗"、"最少取出 21 个"、"答案是 21"
	if matches := reAnswerPrefix.FindAllStringSubmatch(clean, -1); len(matches) > 0 {
		lastMatch := matches[len(matches)-1]
		if len(lastMatch) > 1 {
			if n, err := strconv.Atoi(lastMatch[1]); err == nil {
				return n, true
			}
		}
	}

	// 4. 若正文中含有孤立的 "21"
	if strings.Contains(clean, " 21 ") || strings.Contains(clean, " 21个") || strings.Contains(clean, " 21颗") || strings.HasSuffix(clean, "21") {
		return 21, true
	}

	return 0, false
}

// EvaluateCandyResponse 对模型回答、推理 Token 及输出 TPS 进行综合定性。
func EvaluateCandyResponse(content string, reasoningTokens int64, completionTokens int64, durationMs int64) CandyEvalResult {
	snippet := strings.TrimSpace(content)
	if len(snippet) > 300 {
		snippet = snippet[:300] + "…"
	}

	var tps float64
	if durationMs > 100 && completionTokens > 0 {
		tps = float64(completionTokens) / (float64(durationMs) / 1000.0)
	}

	if strings.TrimSpace(content) == "" {
		return CandyEvalResult{
			Passed:           false,
			Verdict:          CandyVerdictEmpty,
			ReasoningTokens:  reasoningTokens,
			CompletionTokens: completionTokens,
			TPS:              tps,
			Summary:          "上游模型无有效输出内容",
		}
	}

	ans, ok := ExtractCandyAnswer(content)
	if !ok {
		verdict := CandyVerdictDegraded
		sum := "未得出确定数字答案 (判为降智)"
		if tps >= 500 && reasoningTokens == 0 && completionTokens >= 32 {
			verdict = CandyVerdictDegradedTPSSpike
			sum = fmt.Sprintf("降智确诊: 未得出有效数字 且 TPS 异常飙升至 %.0f tok/s (疑似浅层Mini被调换)", tps)
		}
		return CandyEvalResult{
			Passed:           false,
			Verdict:          verdict,
			ExtractedAnswer:  0,
			ReasoningTokens:  reasoningTokens,
			CompletionTokens: completionTokens,
			TPS:              tps,
			ContentSnippet:   snippet,
			Summary:          sum,
		}
	}

	if ans == 21 {
		verdict := CandyVerdictPass
		summary := fmt.Sprintf("正确推导出 21 (思考: %d tok, TPS: %.1f tok/s)", reasoningTokens, tps)
		if reasoningTokens > 0 && reasoningTokens < 50 {
			verdict = CandyVerdictPassLowThinking
			summary = fmt.Sprintf("推导出 21 但思考 Token 偏低 (%d tok, TPS: %.1f tok/s)", reasoningTokens, tps)
		}
		return CandyEvalResult{
			Passed:           true,
			Verdict:          verdict,
			ExtractedAnswer:  21,
			ReasoningTokens:  reasoningTokens,
			CompletionTokens: completionTokens,
			TPS:              tps,
			ContentSnippet:   snippet,
			Summary:          summary,
		}
	}

	verdict := CandyVerdictDegraded
	summary := fmt.Sprintf("推导错误: 得出 %d (标准正解为 21，确诊降智)", ans)
	if tps >= 500 && reasoningTokens == 0 && completionTokens >= 32 {
		verdict = CandyVerdictDegradedTPSSpike
		summary = fmt.Sprintf("降智确诊: 答案=%d(非21) 且 TPS 异常飙升至 %.0f tok/s (出口已被标记/浅层替换)", ans, tps)
	}

	return CandyEvalResult{
		Passed:           false,
		Verdict:          verdict,
		ExtractedAnswer:  ans,
		ReasoningTokens:  reasoningTokens,
		CompletionTokens: completionTokens,
		TPS:              tps,
		ContentSnippet:   snippet,
		Summary:          summary,
	}
}

func newCandyHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 65 * time.Second,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ForceAttemptHTTP2:     true,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}
}

// ProbeCandyAccount 执行单账号糖果测试探测。
func (s *Service) ProbeCandyAccount(ctx context.Context, auth postgres.AccountAuth, model, source string, autoCooldown bool, deferSave bool) map[string]any {
	if model == "" {
		if len(s.Models) > 0 {
			model = s.Models[0]
		} else {
			model = "grok-4.5"
		}
	}
	started := time.Now()
	base := map[string]any{
		"ok":         false,
		"available":  false,
		"probe_type": "candy_eval",
		"account_id": auth.ID,
		"email":      auth.Email,
		"model":      model,
		"probed_at":  started.Unix(),
		"source":     source,
	}

	evalCtx, evalCancel := context.WithTimeout(ctx, 60*time.Second)
	defer evalCancel()

	var proxyURL string
	var boundNodeID string
	if s.Egress != nil {
		if node, _ := s.Egress.SelectNodeForAccount(auth.ID); node != nil {
			proxyURL = node.ProxyURL
			boundNodeID = node.ID
			base["egress_node_id"] = node.ID
			base["egress_redacted"] = node.RedactedURL
		}
	}

	client := &grok.Client{
		BaseURL: s.Upstream,
		HTTP:    newCandyHTTPClient(),
	}

	body := map[string]any{
		"model":          model,
		"stream":         true,
		"max_tokens":     1024,
		"stream_options": map[string]any{"include_usage": true},
		"messages": []any{
			map[string]any{"role": "user", "content": CandyProblemPrompt},
		},
	}

	resp, err := client.Open(evalCtx, grok.Account{ID: auth.ID, Token: auth.Token, ProxyURL: proxyURL}, model, body)
	if err != nil {
		status := 0
		errText := err.Error()
		var ue *grok.UpstreamError
		if errors.As(err, &ue) {
			status = ue.Status
			errText = ue.Body
			if len(errText) > 400 {
				errText = errText[:400]
			}
		}
		base["status_code"] = status
		base["error"] = errText
		base["verdict"] = CandyVerdictError
		lat := time.Since(started).Milliseconds()
		base["latency_ms"] = lat
		if s.Egress != nil && boundNodeID != "" {
			s.Egress.RecordProbeResult(boundNodeID, lat, 0, false, "上游连接或请求报错: "+errText)
		}
		if s.Store != nil && !deferSave {
			_ = s.Store.SaveLastProbe(ctx, auth.ID, base)
		}
		return base
	}
	defer resp.Body.Close()

	var (
		contentBuilder    strings.Builder
		reasoningTokens   int64
		completionTokens  int64
		systemFingerprint string
	)

	_ = grok.ReadSSE(resp.Body, func(ev grok.Event) error {
		if ev.Done || len(ev.Data) == 0 {
			return nil
		}
		var parsed struct {
			SystemFingerprint string `json:"system_fingerprint"`
			Choices           []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				CompletionTokens        int64 `json:"completion_tokens"`
				CompletionTokensDetails struct {
					ReasoningTokens int64 `json:"reasoning_tokens"`
				} `json:"completion_tokens_details"`
			} `json:"usage"`
		}
		if e := json.Unmarshal(ev.Data, &parsed); e == nil {
			if parsed.SystemFingerprint != "" {
				systemFingerprint = parsed.SystemFingerprint
			}
			if parsed.Usage.CompletionTokensDetails.ReasoningTokens > 0 {
				reasoningTokens = parsed.Usage.CompletionTokensDetails.ReasoningTokens
			}
			if parsed.Usage.CompletionTokens > 0 {
				completionTokens = parsed.Usage.CompletionTokens
			}
			for _, choice := range parsed.Choices {
				if choice.Delta.Content != "" {
					contentBuilder.WriteString(choice.Delta.Content)
				} else if choice.Message.Content != "" {
					contentBuilder.WriteString(choice.Message.Content)
				}
			}
		}
		return nil
	})

	content := contentBuilder.String()
	durationMs := time.Since(started).Milliseconds()
	evalResult := EvaluateCandyResponse(content, reasoningTokens, completionTokens, durationMs)

	base["latency_ms"] = durationMs
	base["status_code"] = resp.StatusCode
	base["verdict"] = evalResult.Verdict
	base["summary"] = evalResult.Summary
	base["extracted_answer"] = evalResult.ExtractedAnswer
	base["reasoning_tokens"] = reasoningTokens
	base["completion_tokens"] = completionTokens
	base["tps"] = evalResult.TPS
	if systemFingerprint != "" {
		base["system_fingerprint"] = systemFingerprint
	}
	if len(content) > 300 {
		base["content_snippet"] = content[:300] + "…"
	} else {
		base["content_snippet"] = content
	}

	if evalResult.Passed {
		base["ok"] = true
		base["available"] = true
		if s.Store != nil {
			if _, e := s.Store.ClearAccountCooldown(ctx, auth.ID); e == nil {
				base["recovered"] = true
			}
		}
	} else {
		base["ok"] = false
		base["available"] = false
		base["error"] = evalResult.Summary
		// 判定降智：自动施加惩罚性冷却并移出轮询池（默认 48 小时）
		if autoCooldown && s.Store != nil {
			hours := s.CandyEvalCooldownHours
			if hours <= 0 {
				hours = 48
			}
			coolSec := float64(hours) * 3600.0
			reason := fmt.Sprintf("降智惩罚冷却: 糖果测试未答出21 (实际答案=%d)", evalResult.ExtractedAnswer)
			if _, e := s.Store.KickFromPool(ctx, auth.ID, reason, &coolSec); e == nil {
				base["kicked_cooldown"] = true
			}
		}
	}

	if s.Egress != nil && boundNodeID != "" {
		s.Egress.RecordProbeResult(boundNodeID, durationMs, evalResult.TPS, evalResult.Passed, evalResult.Summary)
	}

	if s.Store != nil && !deferSave {
		_ = s.Store.SaveLastProbe(ctx, auth.ID, base)
	}

	return base
}

// ProbeCandyIDs 批量并发执行糖果测试
func (s *Service) ProbeCandyIDs(ctx context.Context, ids []string, model string, autoCooldown bool, source string) []map[string]any {
	out := make([]map[string]any, 0, len(ids))
	if len(ids) == 0 {
		return out
	}

	type resolved struct {
		id   string
		auth *postgres.AccountAuth
		err  error
	}
	resolvedList := make([]resolved, 0, len(ids))
	for _, id := range ids {
		auth, err := s.Store.GetAccountAuth(ctx, id)
		resolvedList = append(resolvedList, resolved{id: id, auth: auth, err: err})
	}

	// 糖果测试单请求耗时长，控制并发 workers 在 4~12 之间 (默认 8)
	workers := 8
	if s.Workers > 0 {
		workers = s.Workers
		if workers > 12 {
			workers = 12
		}
	}
	if workers > len(resolvedList) {
		workers = len(resolvedList)
	}

	results := make([]map[string]any, len(resolvedList))
	jobs := make(chan int, workers*2)
	var wg sync.WaitGroup

	workerFn := func() {
		defer wg.Done()
		for idx := range jobs {
			r := resolvedList[idx]
			if r.err != nil || r.auth == nil {
				results[idx] = map[string]any{
					"ok": false, "available": false, "probe_type": "candy_eval",
					"account_id": r.id, "error": "account not found in store",
					"verdict": CandyVerdictError,
				}
				continue
			}
			probe := s.ProbeCandyAccount(ctx, *r.auth, model, source, autoCooldown, false)
			results[idx] = probe
		}
	}

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go workerFn()
	}

	for i := range resolvedList {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	for _, res := range results {
		if res != nil {
			out = append(out, res)
		}
	}
	return out
}
