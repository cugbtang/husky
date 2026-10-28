package pglock

import "testing"

func TestWorkflowKeyIsolation(t *testing.T) {
	ids := []uint{0, 1, 2, 42, 1 << 20, 4294967295}
	seen := make(map[int64]uint, len(ids))
	for _, id := range ids {
		key := WorkflowKey(id)
		if key == WorkerKey || key == MigrationKey {
			t.Fatalf("WorkflowKey(%d)=%#x 与单例锁的 key 冲突", id, key)
		}
		if key&WorkflowKeyPrefix != WorkflowKeyPrefix {
			t.Fatalf("WorkflowKey(%d)=%#x 不在 %#x 前缀空间内", id, key, WorkflowKeyPrefix)
		}
		if prev, dup := seen[key]; dup {
			t.Fatalf("WorkflowKey 冲突：workflow %d 与 %d 得到同一个 key %#x", prev, id, key)
		}
		seen[key] = id
	}
}

// 低 32 位被截断时会发生 key 复用，这里把边界行为写成断言，
// 免得以后有人把 workflowID 换成 int64 时以为这里是全量映射。
func TestWorkflowKeyWrapsAbove32Bits(t *testing.T) {
	const id = uint(1 << 32)
	if got, want := WorkflowKey(id), WorkflowKey(0); got != want {
		t.Fatalf("WorkflowKey(%d)=%#x, 期望回绕到 WorkflowKey(0)=%#x", id, got, want)
	}
}
