package egress

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Node 状态枚举
const (
	StatusHealthy     = "healthy"
	StatusQuarantined = "quarantined"
	StatusWarning     = "warning"
)

// Node 表示一个出站代理出口节点
type Node struct {
	ID                string    `json:"id"`
	ProxyURL          string    `json:"-"`
	RedactedURL       string    `json:"redacted_url"`
	Status            string    `json:"status"`
	QuarantinedUntil  time.Time `json:"quarantined_until"`
	QuarantineReason  string    `json:"quarantine_reason"`
	ConsecutiveFails  int       `json:"consecutive_fails"`
	TotalRequests     int64     `json:"total_requests"`
	LastLatencyMs     int64     `json:"last_latency_ms"`
	LastTPS           float64   `json:"last_tps"`
	BoundAccountCount int       `json:"bound_account_count"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// Manager 出口节点与粘性调度管理器
type Manager struct {
	mu            sync.RWMutex
	nodes         map[string]*Node
	orderedIDs    []string
	accountSticky map[string]string // accountID -> nodeID
}

// NewManager 创建出口节点管理器
func NewManager() *Manager {
	return &Manager{
		nodes:         make(map[string]*Node),
		orderedIDs:    make([]string, 0),
		accountSticky: make(map[string]string),
	}
}

// LoadFromConfig 从 settings 中的 outbound_proxy 文本解析加载节点列表
func (m *Manager) LoadFromConfig(proxyText string, defaultUser, defaultPass string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	lines := splitProxyLines(proxyText)
	newNodes := make(map[string]*Node)
	ordered := make([]string, 0, len(lines))

	for idx, line := range lines {
		fullURL, redacted := parseAndCanonicalize(line, defaultUser, defaultPass)
		if fullURL == "" {
			continue
		}
		id := fmt.Sprintf("node-%d", idx+1)
		hash := sha256.Sum256([]byte(fullURL))
		shortHash := hex.EncodeToString(hash[:])[:8]
		id = fmt.Sprintf("node-%s", shortHash)

		// 保留已有状态
		existing, ok := m.nodes[id]
		if ok {
			existing.ProxyURL = fullURL
			existing.RedactedURL = redacted
			existing.UpdatedAt = time.Now()
			newNodes[id] = existing
		} else {
			newNodes[id] = &Node{
				ID:          id,
				ProxyURL:    fullURL,
				RedactedURL: redacted,
				Status:      StatusHealthy,
				UpdatedAt:   time.Now(),
			}
		}
		ordered = append(ordered, id)
	}

	m.nodes = newNodes
	m.orderedIDs = ordered

	// 重新计算绑定的账号
	m.recalculateBindingsLocked()
}

// SelectNodeForAccount 为账号粘性选择一个健康的出口节点（坏出口自动迁移迁出）
func (m *Manager) SelectNodeForAccount(accountID string) (*Node, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	healthyIDs := m.getHealthyNodeIDsLocked()
	if len(healthyIDs) == 0 {
		// 无健康节点，若有节点则兜底返回第一个，否则返回空
		if len(m.orderedIDs) > 0 {
			return m.nodes[m.orderedIDs[0]], false
		}
		return nil, false
	}

	// 检查当前账号粘性绑定的节点是否依然健康
	if currentID, exists := m.accountSticky[accountID]; exists {
		if node, ok := m.nodes[currentID]; ok && node.IsHealthy() {
			return node, true
		}
		// 绑定节点已被隔离或失效，触发动态迁移（Migrate）
	}

	// 基于哈希环为账号确定性分配一个健康节点
	h := sha256.Sum256([]byte(accountID))
	idx := int(h[0]) % len(healthyIDs)
	selectedID := healthyIDs[idx]
	m.accountSticky[accountID] = selectedID
	m.recalculateBindingsLocked()

	return m.nodes[selectedID], true
}

// QuarantineNode 将某个出口节点隔离（例如检测到降智或网络故障），并释放绑定其上的账号
func (m *Manager) QuarantineNode(nodeID string, reason string, duration time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	node, ok := m.nodes[nodeID]
	if !ok {
		return fmt.Errorf("node not found: %s", nodeID)
	}

	node.Status = StatusQuarantined
	node.QuarantinedUntil = time.Now().Add(duration)
	node.QuarantineReason = reason
	node.ConsecutiveFails++
	node.UpdatedAt = time.Now()

	// 将所有原本粘在该坏节点上的账号迁移迁出（解绑，下次请求自动迁移至健康节点）
	for accID, nID := range m.accountSticky {
		if nID == nodeID {
			delete(m.accountSticky, accID)
		}
	}

	m.recalculateBindingsLocked()
	return nil
}

// RestoreNode 提前解除某个节点的隔离
func (m *Manager) RestoreNode(nodeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	node, ok := m.nodes[nodeID]
	if !ok {
		return fmt.Errorf("node not found: %s", nodeID)
	}

	node.Status = StatusHealthy
	node.QuarantinedUntil = time.Time{}
	node.QuarantineReason = ""
	node.ConsecutiveFails = 0
	node.UpdatedAt = time.Now()

	m.recalculateBindingsLocked()
	return nil
}

// RecordProbeResult 记录节点的测智或探测结果
func (m *Manager) RecordProbeResult(nodeID string, latencyMs int64, tps float64, passed bool, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	node, ok := m.nodes[nodeID]
	if !ok {
		return
	}

	node.LastLatencyMs = latencyMs
	node.LastTPS = tps
	node.TotalRequests++
	node.UpdatedAt = time.Now()

	if !passed {
		// 测智失败或 TPS 异常飙升
		node.ConsecutiveFails++
		isTPSSpike := strings.Contains(reason, "TPS") || strings.Contains(reason, "飙升") || strings.Contains(reason, "mini") || strings.Contains(reason, "Mini")
		// 若发生 TPS 异常飙升（典型浅层 Mini 替换）或者出口连续失败达到 3 次，才触发自动隔离 1 小时
		if isTPSSpike || node.ConsecutiveFails >= 3 {
			node.Status = StatusQuarantined
			node.QuarantinedUntil = time.Now().Add(1 * time.Hour)
			node.QuarantineReason = reason
			// 迁出绑定的账号
			for accID, nID := range m.accountSticky {
				if nID == nodeID {
					delete(m.accountSticky, accID)
				}
			}
		}
	} else {
		node.ConsecutiveFails = 0
		if node.Status == StatusQuarantined && time.Now().After(node.QuarantinedUntil) {
			node.Status = StatusHealthy
			node.QuarantineReason = ""
		}
	}
	m.recalculateBindingsLocked()
}

// ListNodes 获取全部节点列表及聚合指标
func (m *Manager) ListNodes() []*Node {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	out := make([]*Node, 0, len(m.orderedIDs))
	for _, id := range m.orderedIDs {
		n := m.nodes[id]
		// 检查隔离是否已自然到期
		if n.Status == StatusQuarantined && !n.QuarantinedUntil.IsZero() && now.After(n.QuarantinedUntil) {
			n.Status = StatusHealthy
			n.QuarantineReason = ""
		}
		copyNode := *n
		out = append(out, &copyNode)
	}
	return out
}

// PrebindAccounts 预先将给定全量账号均匀粘性分配到健康出口节点
func (m *Manager) PrebindAccounts(accountIDs []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	healthyIDs := m.getHealthyNodeIDsLocked()
	if len(healthyIDs) == 0 {
		return
	}

	for _, accID := range accountIDs {
		accID = strings.TrimSpace(accID)
		if accID == "" {
			continue
		}
		h := sha256.Sum256([]byte(accID))
		idx := int(h[0]) % len(healthyIDs)
		m.accountSticky[accID] = healthyIDs[idx]
	}
	m.recalculateBindingsLocked()
}

// Rebalance 强制重平衡所有账号与健康出口的粘性映射
func (m *Manager) Rebalance() {
	m.mu.Lock()
	defer m.mu.Unlock()

	healthyIDs := m.getHealthyNodeIDsLocked()
	if len(healthyIDs) == 0 {
		return
	}

	// 重新哈希映射全部已有账号
	for accID := range m.accountSticky {
		h := sha256.Sum256([]byte(accID))
		idx := int(h[0]) % len(healthyIDs)
		m.accountSticky[accID] = healthyIDs[idx]
	}
	m.recalculateBindingsLocked()
}

// IsHealthy 判断节点当前是否健康可用
func (n *Node) IsHealthy() bool {
	if n.Status == StatusHealthy {
		return true
	}
	if n.Status == StatusQuarantined && !n.QuarantinedUntil.IsZero() && time.Now().After(n.QuarantinedUntil) {
		return true
	}
	return false
}

func (m *Manager) getHealthyNodeIDsLocked() []string {
	now := time.Now()
	var healthy []string
	for _, id := range m.orderedIDs {
		n := m.nodes[id]
		if n.Status == StatusHealthy || (n.Status == StatusQuarantined && !n.QuarantinedUntil.IsZero() && now.After(n.QuarantinedUntil)) {
			healthy = append(healthy, id)
		}
	}
	return healthy
}

func (m *Manager) recalculateBindingsLocked() {
	counts := make(map[string]int)
	for _, nodeID := range m.accountSticky {
		counts[nodeID]++
	}
	for _, node := range m.nodes {
		node.BoundAccountCount = counts[node.ID]
	}
}

func splitProxyLines(text string) []string {
	var lines []string
	for _, part := range strings.Split(text, "\n") {
		for _, sub := range strings.FieldsFunc(part, func(r rune) bool {
			return r == ';' || r == ','
		}) {
			sub = strings.TrimSpace(sub)
			if sub != "" && !strings.HasPrefix(sub, "#") {
				lines = append(lines, sub)
			}
		}
	}
	return lines
}

func parseAndCanonicalize(raw, defUser, defPass string) (fullURL, redacted string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", ""
	}
	if u.User == nil && defUser != "" {
		if defPass != "" {
			u.User = url.UserPassword(defUser, defPass)
		} else {
			u.User = url.User(defUser)
		}
	}
	fullURL = u.String()

	// 脱敏
	redactedU := *u
	if redactedU.User != nil {
		username := redactedU.User.Username()
		redactedU.User = url.UserPassword(username, "******")
	}
	redacted = redactedU.String()
	return fullURL, redacted
}
