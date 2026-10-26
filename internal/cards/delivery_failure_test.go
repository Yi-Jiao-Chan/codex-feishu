package cards

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDeliveryFailureContainsOnlySafeDiagnosticMetadata(t *testing.T) {
	card := DeliveryFailure(230099, 400, "req-safe_123", "attempt-safe")
	var document map[string]any
	if err := json.Unmarshal([]byte(card), &document); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(card, "最终回复发送失败") || !strings.Contains(card, "code 230099") ||
		!strings.Contains(card, "HTTP 400") || !strings.Contains(card, "req-safe_123") ||
		strings.Contains(card, "attempt-safe") {
		t.Fatalf("failure card lost safe remote evidence: %s", card)
	}
	unsafe := DeliveryFailure(1, 400, "req/private/path", "attempt-safe")
	if strings.Contains(unsafe, "req/private/path") || !strings.Contains(unsafe, "attempt-safe") {
		t.Fatalf("failure card exposed an unsafe request id: %s", unsafe)
	}
}
