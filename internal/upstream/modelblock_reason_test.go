package upstream

import "testing"

// TestModelBlockReasonFor 11102 与 11158 的负缓存 reason 分开，便于台账区分原因。
func TestModelBlockReasonFor(t *testing.T) {
	if got := ModelBlockReasonFor(`{"code":11102,"msg":"service info not found"}`); got != ModelBlockReason {
		t.Errorf("11102 reason=%q want %q", got, ModelBlockReason)
	}
	if got := ModelBlockReasonFor(`{"code":11158,"msg":"model [x] is not allowed for user type [personal]"}`); got != ModelForbiddenReason {
		t.Errorf("11158 reason=%q want %q", got, ModelForbiddenReason)
	}
	if got := ModelBlockReasonFor(`{"msg":"is not allowed for user type"}`); got != ModelForbiddenReason {
		t.Errorf("11158 marker reason=%q want %q", got, ModelForbiddenReason)
	}
}
