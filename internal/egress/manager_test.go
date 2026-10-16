package egress

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEgressManagerStickyAndMigration(t *testing.T) {
	mgr := NewManager()
	proxyText := `http://1.1.1.1:8080
http://2.2.2.2:8080
http://3.3.3.3:8080`

	mgr.LoadFromConfig(proxyText, "", "")
	nodes := mgr.ListNodes()
	if len(nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(nodes))
	}

	acc1 := "acc-1001"
	nodeA, ok := mgr.SelectNodeForAccount(acc1)
	if !ok || nodeA == nil {
		t.Fatalf("expected node selection to succeed")
	}

	// 再次为该账号选节点，由于粘性绑定应选回同一节点
	nodeB, _ := mgr.SelectNodeForAccount(acc1)
	if nodeA.ID != nodeB.ID {
		t.Fatalf("expected sticky affinity to same node, got %s and %s", nodeA.ID, nodeB.ID)
	}

	// 模拟该出口节点被隔离 (例如糖果测智未得出21或者TPS异常飙升)
	quarantineReason := "糖果测试降智拦截: 得出36，TPS异常飙升至 1024 tok/s"
	if err := mgr.QuarantineNode(nodeA.ID, quarantineReason, 1*time.Hour); err != nil {
		t.Fatalf("quarantine failed: %v", err)
	}

	// 验证坏节点已被隔离，账号自动迁移迁出到其他健康节点
	nodeC, ok := mgr.SelectNodeForAccount(acc1)
	if !ok || nodeC == nil {
		t.Fatalf("expected migration to succeed")
	}
	if nodeC.ID == nodeA.ID {
		t.Fatalf("expected account to migrate away from quarantined node %s", nodeA.ID)
	}

	// 恢复该节点
	if err := mgr.RestoreNode(nodeA.ID); err != nil {
		t.Fatalf("restore failed: %v", err)
	}

	// 验证 JSON 序列化绝对不能包含 proxy_url 明文凭据
	rawJSON, err := json.Marshal(nodeA)
	if err != nil {
		t.Fatalf("json marshal node failed: %v", err)
	}
	if strings.Contains(string(rawJSON), "proxy_url") {
		t.Fatalf("security violation: proxy_url must not be exported in json, got %s", string(rawJSON))
	}
}
