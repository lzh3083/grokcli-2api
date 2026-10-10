package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	ErrorQualityDegraded                   = "quality_degraded"
	qualityRetryFailOpen                   = "fail_open"
	qualityRetryFailClosed                 = "fail_closed"
	defaultQualityMaxAttempts              = 6
	defaultQualityHoldTimeout              = 30 * time.Second
	defaultQualityMinOutput                = int64(8)
	defaultMinEncryptedBytes               = 256
	defaultEncryptedBytesPerReasoningToken = 4
	defaultBurstFlushMS                    = int64(1000)
	defaultBurstMaxVisible                 = int64(32)
	defaultBurstMinReasoning               = int64(80)
	// Fake encrypted thinking dumps the whole visible answer after a long
	// wait. Catch flush windows up to 2s so 1.8s / 1962-token dumps are withheld too.
	defaultFakeEncFlushMS = int64(2000)
	// Cipher-only "thinking" that is already dumping this much visible text
	// with usage.reasoning_tokens=0 is the 128k status-loop drool, not a
	// real encrypted thinking stream.
	defaultCipherDroolVisible      = int64(1024)
	defaultMissingThinkingCooldown = 12 * time.Hour
	// An empty stream that idles while held is treated as an account-quality
	// failure: the request can still rotate before any bytes reach the client.
	qualityIdleAccountCooldown = 15 * time.Minute
)

var (
	errQualityDegraded    = errors.New("Upstream degraded: missing reasoning / thinking evidence (quality_degraded)")
	errQualityEmptyStream = errors.New("Upstream returned HTTP 200 with empty model output (no content/tool_calls)")
)

// QualityUsage captures token counts observed during stream quality scanning.
type QualityUsage struct {
	Reported        bool
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	TotalTokens     int64
	ResponseModel   string
}

// QualityRetryRuntime is the isolated request-path withhold/retry policy.
type QualityRetryRuntime struct {
	Enabled                         bool
	MaxAttempts                     int
	HoldTimeout                     time.Duration
	MinOutputTokens                 int64
	OnExhausted                     string
	AccountCooldown                 time.Duration
	IdleAccountCooldown             time.Duration
	MinEncryptedBytes               int
	EncryptedBytesPerReasoningToken int
}

// QualityStreamSignals is the hold classifier input.
type QualityStreamSignals struct {
	HasThinking bool
	// PlaintextThinking is reasoning_text / summary deltas. Encrypted
	// ciphertext can set HasThinking without this bit.
	PlaintextThinking bool
	// ReasoningStarted is an empty reasoning item or the Chat SSE stub
	// `: grok2api-reasoning-start`. That is not proof of thinking: 降智
	// still emits the stub, then dumps visible tokens with usage 0.
	ReasoningStarted bool
	VisibleTokens    int64
	ReasoningTokens  int64
	OutputTokens     int64
	EncryptedBytes   int
	FirstVisible     bool
	VisibleFlushMS   int64
	Terminal         bool
	HoldExpired      bool
}

// QualityVerdict is the hold decision for one upstream stream.
type QualityVerdict string

const (
	QualityWait     QualityVerdict = "wait"
	QualityDeliver  QualityVerdict = "deliver"
	QualityWithhold QualityVerdict = "withhold"
)

// QualityRetryAction is what the attempt loop does with a withhold verdict.
type QualityRetryAction string

const (
	QualityActionDeliver     QualityRetryAction = "deliver"
	QualityActionDeliverLast QualityRetryAction = "deliver_last"
	QualityActionRetry       QualityRetryAction = "retry"
	QualityActionReject      QualityRetryAction = "reject"
)

// DefaultQualityRetryRuntime loads default runtime settings with optional env overrides.
func DefaultQualityRetryRuntime() QualityRetryRuntime {
	enabled := true
	if raw := strings.TrimSpace(os.Getenv("GROK2API_QUALITY_HOLD_ENABLED")); raw != "" {
		enabled = raw != "0" && !strings.EqualFold(raw, "false") && !strings.EqualFold(raw, "no")
	}
	maxAttempts := defaultQualityMaxAttempts
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("GROK2API_QUALITY_MAX_ATTEMPTS"))); err == nil && v > 0 {
		maxAttempts = v
	}
	holdTimeout := defaultQualityHoldTimeout
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("GROK2API_QUALITY_HOLD_TIMEOUT_SEC"))); err == nil && v > 0 {
		holdTimeout = time.Duration(v) * time.Second
	}
	minOutput := defaultQualityMinOutput
	if v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("GROK2API_QUALITY_MIN_OUTPUT_TOKENS")), 10, 64); err == nil && v > 0 {
		minOutput = v
	}
	onExhausted := strings.TrimSpace(os.Getenv("GROK2API_QUALITY_ON_EXHAUSTED"))
	return normalizeQualityRetry(QualityRetryRuntime{
		Enabled:                         enabled,
		MaxAttempts:                     maxAttempts,
		HoldTimeout:                     holdTimeout,
		MinOutputTokens:                 minOutput,
		OnExhausted:                     onExhausted,
		AccountCooldown:                 defaultMissingThinkingCooldown,
		IdleAccountCooldown:             qualityIdleAccountCooldown,
		MinEncryptedBytes:               defaultMinEncryptedBytes,
		EncryptedBytesPerReasoningToken: defaultEncryptedBytesPerReasoningToken,
	})
}

func normalizeQualityRetry(cfg QualityRetryRuntime) QualityRetryRuntime {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultQualityMaxAttempts
	}
	if cfg.HoldTimeout <= 0 {
		cfg.HoldTimeout = defaultQualityHoldTimeout
	}
	if cfg.MinOutputTokens <= 0 {
		cfg.MinOutputTokens = defaultQualityMinOutput
	}
	if cfg.AccountCooldown <= 0 {
		cfg.AccountCooldown = defaultMissingThinkingCooldown
	}
	if cfg.IdleAccountCooldown <= 0 {
		cfg.IdleAccountCooldown = qualityIdleAccountCooldown
	}
	if cfg.MinEncryptedBytes <= 0 {
		cfg.MinEncryptedBytes = defaultMinEncryptedBytes
	}
	if cfg.EncryptedBytesPerReasoningToken <= 0 {
		cfg.EncryptedBytesPerReasoningToken = defaultEncryptedBytesPerReasoningToken
	}
	cfg.OnExhausted = normalizeQualityExhaustionPolicy(cfg.OnExhausted)
	return cfg
}

// encryptedThinkingFloor is max(minBytes, reasoningTokens*bytesPerToken).
// A non-empty stub such as "gAAAA-cipher" is not thinking.
func encryptedThinkingFloor(minBytes, bytesPerToken int, reasoningTokens int64) int {
	if minBytes <= 0 {
		minBytes = defaultMinEncryptedBytes
	}
	if bytesPerToken <= 0 {
		bytesPerToken = defaultEncryptedBytesPerReasoningToken
	}
	floor := minBytes
	if reasoningTokens > 0 {
		need := int(reasoningTokens) * bytesPerToken
		if need > floor {
			floor = need
		}
	}
	return floor
}

func qualityFastFlush(sig QualityStreamSignals, limitMS int64) bool {
	return sig.FirstVisible && sig.VisibleFlushMS >= 0 && sig.VisibleFlushMS < limitMS
}

func qualityMeetsEncryptedFloor(sig QualityStreamSignals) bool {
	if sig.EncryptedBytes <= 0 {
		return false
	}
	return sig.EncryptedBytes >= encryptedThinkingFloor(0, 0, sig.ReasoningTokens)
}

func qualityHasDumpBill(sig QualityStreamSignals) bool {
	return sig.ReasoningTokens >= defaultBurstMinReasoning || qualityMeetsEncryptedFloor(sig)
}

func qualityIsBurstDump(sig QualityStreamSignals, minOutput int64) bool {
	_ = minOutput
	if sig.PlaintextThinking || qualityMeetsEncryptedFloor(sig) {
		return false
	}
	visible := sig.VisibleTokens
	heavyReasoning := sig.ReasoningTokens >= defaultBurstMinReasoning
	shortVisible := visible > 0 && visible < defaultBurstMaxVisible
	// Hold timed out, then a short greeting dumped with a large reasoning bill
	// (TUI "你好" after 30s / 954 thinking tokens).
	if sig.HoldExpired && shortVisible && heavyReasoning {
		return true
	}
	if qualityFastFlush(sig, defaultBurstFlushMS) && qualityHasDumpBill(sig) {
		return true
	}
	return false
}

// qualityIsFakeEncryptedDump is the 18190 / 18183 dump: a large reasoning
// bill without valid ciphertext floor, then the visible answer arrives in <2s.
func qualityIsFakeEncryptedDump(sig QualityStreamSignals, minOutput int64) bool {
	_ = minOutput
	if sig.PlaintextThinking || qualityMeetsEncryptedFloor(sig) {
		return false
	}
	if !qualityFastFlush(sig, defaultFakeEncFlushMS) {
		return false
	}
	return qualityHasDumpBill(sig)
}

// qualityIsFastReasoningRatioDump catches plaintext thinking that is still a
// 1ms dump: billed reasoning is >=80% of output and the visible flush is <2s.
func qualityIsFastReasoningRatioDump(sig QualityStreamSignals) bool {
	if !sig.PlaintextThinking {
		return false
	}
	if !qualityFastFlush(sig, defaultFakeEncFlushMS) {
		return false
	}
	output := sig.OutputTokens
	if output <= 0 {
		output = sig.VisibleTokens + sig.ReasoningTokens
	}
	if output <= 0 || sig.ReasoningTokens <= 0 {
		return false
	}
	return sig.ReasoningTokens*5 >= output*4
}

// qualityIsCipherDrool is the 128k TUI status-loop: ciphertext met the
// floor so HasThinking is true, but there is no plaintext reasoning and
// usage.reasoning_tokens is still 0 while visible text is already dumping.
func qualityIsCipherDrool(sig QualityStreamSignals, minOutput int64) bool {
	if minOutput <= 0 {
		minOutput = defaultQualityMinOutput
	}
	if sig.PlaintextThinking || sig.ReasoningTokens > 0 {
		return false
	}
	if sig.EncryptedBytes <= 0 {
		return false
	}
	visible := sig.VisibleTokens
	if visible >= defaultCipherDroolVisible {
		return true
	}
	if sig.Terminal && visible >= minOutput {
		return true
	}
	return false
}

// ClassifyQualityHold decides whether a held stream may be forwarded.
func ClassifyQualityHold(sig QualityStreamSignals, minOutput int64) QualityVerdict {
	if minOutput <= 0 {
		minOutput = defaultQualityMinOutput
	}
	if qualityIsBurstDump(sig, minOutput) || qualityIsCipherDrool(sig, minOutput) || qualityIsFakeEncryptedDump(sig, minOutput) || qualityIsFastReasoningRatioDump(sig) {
		return QualityWithhold
	}
	if sig.HasThinking {
		return QualityDeliver
	}
	// Prefer observed/derived visible output. Total output includes reasoning
	// tokens, which are deliberately not trusted as quality evidence above.
	output := sig.VisibleTokens
	if output <= 0 {
		output = sig.OutputTokens
	}
	enough := output >= minOutput
	if sig.ReasoningStarted && !sig.Terminal && !sig.HoldExpired {
		return QualityWait
	}
	if sig.Terminal {
		if output <= 0 {
			return QualityWait
		}
		if enough {
			return QualityWithhold
		}
		return QualityDeliver
	}
	if enough {
		return QualityWithhold
	}
	if sig.HoldExpired {
		if output <= 0 {
			return QualityWait
		}
		if enough {
			return QualityWithhold
		}
		return QualityDeliver
	}
	return QualityWait
}

func normalizeQualityExhaustionPolicy(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), qualityRetryFailOpen) {
		return qualityRetryFailOpen
	}
	return qualityRetryFailClosed
}

// DecideQualityRetry caps withhold recovery at maxAttempts (default 6).
func DecideQualityRetry(verdict QualityVerdict, attemptIndex, maxAttempts int, onExhausted string) QualityRetryAction {
	if verdict != QualityWithhold {
		return QualityActionDeliver
	}
	if maxAttempts <= 0 {
		maxAttempts = defaultQualityMaxAttempts
	}
	if attemptIndex < 0 {
		attemptIndex = 0
	}
	if attemptIndex < maxAttempts-1 {
		return QualityActionRetry
	}
	if normalizeQualityExhaustionPolicy(onExhausted) == qualityRetryFailClosed {
		return QualityActionReject
	}
	return QualityActionDeliverLast
}

// BoundQualityRetry turns a Retry into DeliverLast/Reject when the routing
// loop has no remaining account slot.
func BoundQualityRetry(action QualityRetryAction, hasNextRoutingAttempt bool, onExhausted string) QualityRetryAction {
	if action != QualityActionRetry || hasNextRoutingAttempt {
		return action
	}
	if normalizeQualityExhaustionPolicy(onExhausted) == qualityRetryFailClosed {
		return QualityActionReject
	}
	return QualityActionDeliverLast
}

// QualityCommit is the single attempt-loop decision for a held stream.
type QualityCommit struct {
	Action   QualityRetryAction
	Audit    bool
	KeepBody bool
}

// CommitQualityHold is the shipped withhold/retry/commit unit.
func CommitQualityHold(verdict QualityVerdict, qualityAttempt, maxAttempts int, hasNextRouting bool, onExhausted string) QualityCommit {
	action := BoundQualityRetry(
		DecideQualityRetry(verdict, qualityAttempt, maxAttempts, onExhausted),
		hasNextRouting,
		onExhausted,
	)
	switch action {
	case QualityActionRetry, QualityActionReject:
		return QualityCommit{Action: action, Audit: true, KeepBody: false}
	case QualityActionDeliverLast:
		return QualityCommit{Action: action, Audit: false, KeepBody: true}
	default:
		return QualityCommit{Action: QualityActionDeliver, Audit: false, KeepBody: true}
	}
}

// qualityRequestDisablesReasoning checks whether a JSON body map explicitly disables reasoning/thinking.
func qualityRequestDisablesReasoningMap(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	if s, ok := payload["reasoning_effort"].(string); ok && strings.EqualFold(strings.TrimSpace(s), "none") {
		return true
	}
	for _, key := range []string{"reasoning", "output_config", "thinking"} {
		nested, ok := payload[key].(map[string]any)
		if !ok || nested == nil {
			continue
		}
		if s, ok := nested["effort"].(string); ok && strings.EqualFold(strings.TrimSpace(s), "none") {
			return true
		}
		if s, ok := nested["type"].(string); ok && strings.EqualFold(strings.TrimSpace(s), "disabled") {
			return true
		}
		if b, ok := asInt(nested["budget_tokens"]); ok && b == 0 {
			if _, exists := nested["budget_tokens"]; exists {
				return true
			}
		}
	}
	if s, ok := payload["thinking"].(string); ok && strings.EqualFold(strings.TrimSpace(s), "disabled") {
		return true
	}
	return false
}

func qualityRequestDisablesReasoningBytes(body []byte) bool {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	return qualityRequestDisablesReasoningMap(payload)
}

func qualityPeekAbortError(ctx context.Context, err error) error {
	if err != nil {
		return err
	}
	if ctx != nil {
		return ctx.Err()
	}
	return nil
}
