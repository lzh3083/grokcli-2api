package modelhealth

import (
	"testing"
)

func TestExtractCandyAnswer(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    int
		wantOk  bool
	}{
		{
			name: "final line 21",
			content: `首先我们分析题意：
假设取 C 个圆形糖果和 S 个五角星糖果。
...
因此最少需要取出的糖果数目为：
21`,
			want:   21,
			wantOk: true,
		},
		{
			name: "bold 21 in markdown",
			content: `综合以上分析，取 9 个圆形和 12 个五角星形是最优解。
所以最少取出 **21** 个糖果。`,
			want:   21,
			wantOk: true,
		},
		{
			name: "conclusion phrase",
			content: `根据抽屉原理计算，至少需要取出 21 颗才能保证要求成立。`,
			want:   21,
			wantOk: true,
		},
		{
			name: "wrong answer 36",
			content: `最坏情况下需要把所有不需要的都取出来。
计算得出答案为 36。
36`,
			want:   36,
			wantOk: true,
		},
		{
			name: "wrong answer 18 with punctuation",
			content: `经过严密计算：
18。`,
			want:   18,
			wantOk: true,
		},
		{
			name:    "empty response",
			content: "",
			want:    0,
			wantOk:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ExtractCandyAnswer(tc.content)
			if ok != tc.wantOk {
				t.Fatalf("ExtractCandyAnswer() ok = %v, want %v", ok, tc.wantOk)
			}
			if got != tc.want {
				t.Fatalf("ExtractCandyAnswer() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEvaluateCandyResponse(t *testing.T) {
	// 正常通过
	resPass := EvaluateCandyResponse("推导完成，答案是：\n21", 520, 100, 2000)
	if !resPass.Passed || resPass.Verdict != CandyVerdictPass || resPass.ExtractedAnswer != 21 {
		t.Fatalf("expected pass, got %+v", resPass)
	}

	// 答案是 21 但推理 token 偏低（低思考）
	resLowThinking := EvaluateCandyResponse("21", 10, 50, 1000)
	if !resLowThinking.Passed || resLowThinking.Verdict != CandyVerdictPassLowThinking {
		t.Fatalf("expected pass low thinking, got %+v", resLowThinking)
	}

	// 错误答案 36 确诊降智
	resDegraded := EvaluateCandyResponse("答案是 36", 400, 100, 1500)
	if resDegraded.Passed || resDegraded.Verdict != CandyVerdictDegraded || resDegraded.ExtractedAnswer != 36 {
		t.Fatalf("expected degraded, got %+v", resDegraded)
	}

	// TPS 异常飙升+无思考降智 (如 1000 Token/s, 答案算错)
	resTPS := EvaluateCandyResponse("答案是 36", 0, 500, 500)
	if resTPS.Passed || resTPS.Verdict != CandyVerdictDegradedTPSSpike {
		t.Fatalf("expected degraded tps spike, got %+v", resTPS)
	}

	// 空内容
	resEmpty := EvaluateCandyResponse("", 0, 0, 100)
	if resEmpty.Passed || resEmpty.Verdict != CandyVerdictEmpty {
		t.Fatalf("expected empty, got %+v", resEmpty)
	}
}
