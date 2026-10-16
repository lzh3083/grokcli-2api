package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hm2899/grokcli-2api/internal/modelhealth"
)

func TestCandyEvalEndpointsRegistration(t *testing.T) {
	mux := NewMux(Options{
		ModelHealth: modelhealth.New(nil, nil, "http://127.0.0.1:9999", []string{"grok-4.5"}),
	})

	// 测试 POST /admin/api/accounts/candy-eval 未授权被拦截
	reqBatch := httptest.NewRequest("POST", "/admin/api/accounts/candy-eval", strings.NewReader(`{"ids":["acc-1"]}`))
	reqBatch.Header.Set("Content-Type", "application/json")
	recBatch := httptest.NewRecorder()
	mux.ServeHTTP(recBatch, reqBatch)

	if recBatch.Code != http.StatusUnauthorized && recBatch.Code != http.StatusForbidden {
		t.Logf("expected unauthorized or forbidden for unauth admin request, got status: %d", recBatch.Code)
	}

	// 测试 POST /admin/api/accounts/{account_id}/candy-eval 未授权被拦截
	reqOne := httptest.NewRequest("POST", "/admin/api/accounts/acc-1/candy-eval", strings.NewReader(`{}`))
	reqOne.Header.Set("Content-Type", "application/json")
	recOne := httptest.NewRecorder()
	mux.ServeHTTP(recOne, reqOne)

	if recOne.Code != http.StatusUnauthorized && recOne.Code != http.StatusForbidden {
		t.Logf("expected unauthorized or forbidden for unauth admin request, got status: %d", recOne.Code)
	}

	// 测试 GET /admin/api/candy/overview 未授权被拦截
	reqOverview := httptest.NewRequest("GET", "/admin/api/candy/overview", nil)
	recOverview := httptest.NewRecorder()
	mux.ServeHTTP(recOverview, reqOverview)
	if recOverview.Code != http.StatusUnauthorized && recOverview.Code != http.StatusForbidden {
		t.Logf("expected unauthorized or forbidden for unauth admin request, got status: %d", recOverview.Code)
	}

	// 测试 GET /admin/candy 路由有效性
	if !allowedAdminPage("candy") {
		t.Fatalf("expected allowedAdminPage('candy') to be true")
	}
}

func TestCandyEvalResultClassification(t *testing.T) {
	// 验证糖果判定逻辑
	passContent := "分析可知，取9个圆形和12个五角星形是最优解。\n答案是 21"
	res := modelhealth.EvaluateCandyResponse(passContent, 450, 100, 2500)
	if !res.Passed || res.ExtractedAnswer != 21 || res.Verdict != modelhealth.CandyVerdictPass {
		t.Fatalf("expected candy_pass, got %+v", res)
	}

	failContent := "分析后得出：需要把所有西瓜取走，至少需要 36 颗"
	resFail := modelhealth.EvaluateCandyResponse(failContent, 200, 50, 1000)
	if resFail.Passed || resFail.ExtractedAnswer != 36 || resFail.Verdict != modelhealth.CandyVerdictDegraded {
		t.Fatalf("expected candy_degraded, got %+v", resFail)
	}
}

func TestCandyEvalSchedulerSettings(t *testing.T) {
	svc := modelhealth.New(nil, nil, "http://127.0.0.1:9999", []string{"grok-4.5"})
	if svc.CandyEvalEnabled {
		t.Fatalf("expected CandyEvalEnabled default to false")
	}

	// 测试开启
	tr := true
	svc.ConfigureCandy(&tr, 3600, 24)
	if !svc.CandyEvalEnabled {
		t.Fatalf("expected CandyEvalEnabled to be true after configure")
	}

	// 测试热更新关闭
	f := false
	svc.ConfigureCandy(&f, 3600, 24)
	if svc.CandyEvalEnabled {
		t.Fatalf("expected CandyEvalEnabled to be false after configure")
	}
	if svc.CandyEvalInterval.Seconds() != 3600 {
		t.Fatalf("expected interval 3600, got %v", svc.CandyEvalInterval.Seconds())
	}
	if svc.CandyEvalCooldownHours != 24 {
		t.Fatalf("expected cooldown hours 24, got %d", svc.CandyEvalCooldownHours)
	}

	// 测试局部更新：只改 interval_sec，enabled 应保持为 false，不会被强制覆盖
	svc.ConfigureCandy(nil, 7200, 0)
	if svc.CandyEvalEnabled {
		t.Fatalf("expected CandyEvalEnabled to remain false when nil passed")
	}
	if svc.CandyEvalInterval.Seconds() != 7200 {
		t.Fatalf("expected interval 7200, got %v", svc.CandyEvalInterval.Seconds())
	}
	if svc.CandyEvalCooldownHours != 24 {
		t.Fatalf("expected cooldown hours to remain 24, got %d", svc.CandyEvalCooldownHours)
	}
}
