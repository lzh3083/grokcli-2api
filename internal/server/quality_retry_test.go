package server

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func sseLines(lines ...string) string {
	var sb strings.Builder
	for _, line := range lines {
		if strings.HasPrefix(line, ":") {
			sb.WriteString(line + "\n\n")
		} else {
			sb.WriteString("data: " + line + "\n\n")
		}
	}
	return sb.String()
}

func TestClassifyQualityHold(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		sig  QualityStreamSignals
		want QualityVerdict
	}{
		{
			name: "plaintext thinking delivers immediately",
			sig:  QualityStreamSignals{HasThinking: true, PlaintextThinking: true, VisibleTokens: 10},
			want: QualityDeliver,
		},
		{
			name: "usage reasoning tokens alone withhold when no thinking streamed",
			sig:  QualityStreamSignals{ReasoningTokens: 100, VisibleTokens: 80, Terminal: true},
			want: QualityWithhold,
		},
		{
			name: "visible 32 no think withhold midstream",
			sig:  QualityStreamSignals{VisibleTokens: 32},
			want: QualityWithhold,
		},
		{
			name: "short reply no think delivers on terminal",
			sig:  QualityStreamSignals{VisibleTokens: 5, Terminal: true},
			want: QualityDeliver,
		},
		{
			name: "stub midstream waits for usage",
			sig:  QualityStreamSignals{ReasoningStarted: true, VisibleTokens: 64},
			want: QualityWait,
		},
		{
			name: "stub terminal enough visible withhold",
			sig:  QualityStreamSignals{ReasoningStarted: true, VisibleTokens: 64, Terminal: true},
			want: QualityWithhold,
		},
		{
			name: "empty terminal waits for empty handler",
			sig:  QualityStreamSignals{Terminal: true},
			want: QualityWait,
		},
		{
			name: "burst dump after hold expired with heavy reasoning withhold",
			sig:  QualityStreamSignals{HoldExpired: true, VisibleTokens: 10, ReasoningTokens: 954},
			want: QualityWithhold,
		},
		{
			name: "fast flush with dump bill withhold",
			sig:  QualityStreamSignals{FirstVisible: true, VisibleFlushMS: 100, ReasoningTokens: 120, VisibleTokens: 20},
			want: QualityWithhold,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyQualityHold(tc.sig, 8)
			if got != tc.want {
				t.Fatalf("ClassifyQualityHold() = %v, want %v (signals: %#v)", got, tc.want, tc.sig)
			}
		})
	}
}

func TestObserveQualityChunkEmptyReasoningStubIsNotThinking(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("word ", 40)
	chat := qualityScanState{protocol: qualityProtocolChat}
	ObserveQualityChunk(&chat, []byte(sseLines(
		": grok2api-reasoning-start",
		`{"choices":[{"delta":{"content":"`+content+`"}}]}`,
		`{"usage":{"completion_tokens":45,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		"[DONE]",
	)))
	chatSig := chat.signals()
	if chatSig.HasThinking {
		t.Fatalf("chat SSE stub must not count as thinking: %#v", chatSig)
	}
	if !chatSig.ReasoningStarted || !chatSig.Terminal || chatSig.ReasoningTokens != 0 {
		t.Fatalf("chat stub signals = %#v", chatSig)
	}
	if ClassifyQualityHold(chatSig, 8) != QualityWithhold {
		t.Fatalf("chat stub + 0 reasoning must withhold, got %s", ClassifyQualityHold(chatSig, 8))
	}
}

func TestObserveQualityChunkCiphertextFloor(t *testing.T) {
	t.Parallel()
	// 1. Short ciphertext stub (< 256 bytes) does NOT count as thinking
	shortState := qualityScanState{
		protocol:                        qualityProtocolResponses,
		minEncryptedBytes:               256,
		encryptedBytesPerReasoningToken: 4,
	}
	ObserveQualityChunk(&shortState, []byte(sseLines(
		`{"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"gAAAA-short-cipher-stub"}}`,
		`{"type":"response.output_text.delta","delta":"hello world how are you today doing"}`,
		`{"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":20,"output_tokens_details":{"reasoning_tokens":0}}}}`,
	)))
	shortSig := shortState.signals()
	if shortSig.HasThinking {
		t.Fatalf("short stub below 256 bytes must not count as thinking: %#v", shortSig)
	}
	if ClassifyQualityHold(shortSig, 8) != QualityWithhold {
		t.Fatalf("short stub must withhold, got %s", ClassifyQualityHold(shortSig, 8))
	}

	// 2. Ciphertext >= floor (e.g. 300 bytes) counts as thinking and delivers on terminal
	longCipher := strings.Repeat("a", 300)
	longState := qualityScanState{
		protocol:                        qualityProtocolResponses,
		minEncryptedBytes:               256,
		encryptedBytesPerReasoningToken: 4,
		// Simulate streamed visible text lasting > 2s to avoid fake-enc flush trigger
		firstVisibleAt: time.Now().Add(-3 * time.Second),
	}
	ObserveQualityChunk(&longState, []byte(sseLines(
		`{"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"`+longCipher+`"}}`,
		`{"type":"response.output_text.delta","delta":"hello world here is the answer"}`,
		`{"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":30,"output_tokens_details":{"reasoning_tokens":50}}}}`,
	)))
	longSig := longState.signals()
	if !longSig.HasThinking {
		t.Fatalf("ciphertext meeting 256 floor must count as thinking: %#v", longSig)
	}
	if ClassifyQualityHold(longSig, 8) != QualityDeliver {
		t.Fatalf("valid ciphertext must deliver, got %s", ClassifyQualityHold(longSig, 8))
	}
}

func TestPeekQualityStreamEmptyCompletedRetriesWithoutIdle(t *testing.T) {
	t.Parallel()
	started := time.Now()
	replay, verdict, _, _, err := peekQualityStream(
		context.Background(),
		io.NopCloser(strings.NewReader(sseLines(
			`{"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":0}}}`,
		))),
		qualityProtocolResponses,
		QualityRetryRuntime{MinOutputTokens: 8, HoldTimeout: 2 * time.Second},
	)
	if replay != nil {
		defer replay.Close()
	}
	if !errors.Is(err, errQualityEmptyStream) {
		t.Fatalf("peek error = %v, want errQualityEmptyStream", err)
	}
	if verdict != QualityWait {
		t.Fatalf("verdict = %s, want wait", verdict)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("empty completed stream waited %s, want immediate retry", time.Since(started))
	}
}

func TestPeekQualityStreamPlaintextThinkingDeliversAndReplays(t *testing.T) {
	t.Parallel()
	inputSSE := sseLines(
		`{"type":"response.reasoning_text.delta","delta":"let me analyze step by step"}`,
		`{"type":"response.output_text.delta","delta":"here is the conclusion"}`,
		`{"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":50,"output_tokens_details":{"reasoning_tokens":30}}}}`,
	)
	replay, verdict, _, _, err := peekQualityStream(
		context.Background(),
		io.NopCloser(strings.NewReader(inputSSE)),
		qualityProtocolResponses,
		QualityRetryRuntime{MinOutputTokens: 8, HoldTimeout: 2 * time.Second},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verdict != QualityDeliver {
		t.Fatalf("verdict = %s, want QualityDeliver", verdict)
	}
	replayedBytes, err := io.ReadAll(replay)
	if err != nil {
		t.Fatalf("read replay error: %v", err)
	}
	if !strings.Contains(string(replayedBytes), "let me analyze step by step") {
		t.Fatalf("replay missing prefix text: %s", string(replayedBytes))
	}
}
