package cards

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRequestMarkerSerializedBudget(t *testing.T) {
	base := Message("AI", "Codex", "public code <tag>\n你好")
	wire, marked, err := WithRequestMarker(base, "cABCDEFGHIJKLMNOPQRS")
	if err != nil || !marked || len(wire)-len(base) != MaxRequestMarkerBytes {
		t.Fatal("request metadata byte reserve is inaccurate", marked, err, len(wire)-len(base))
	}
}

func TestWithRequestMarkerMarksFirstFreeMarkdown(t *testing.T) {
	const marker = "cabcdefghijklmnopqrs"
	for _, test := range []struct {
		name string
		base string
	}{
		{"message", Message("AI", "Codex", "reply")},
		{"queue", Queue("received", "token")},
		{"progress", Progress("running", "details")},
		{"one element", Progress("running", "")},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, marked, err := WithRequestMarker(test.base, marker)
			if err != nil || !marked {
				t.Fatalf("WithRequestMarker() = marked %v, err %v", marked, err)
			}
			var card map[string]any
			if err := json.Unmarshal([]byte(got), &card); err != nil {
				t.Fatal(err)
			}
			elements := card["body"].(map[string]any)["elements"].([]any)
			if gotID := elements[0].(map[string]any)["element_id"]; gotID != marker {
				t.Fatalf("first markdown marker = %v, want %q", gotID, marker)
			}
			if !reflect.DeepEqual(markerVisibleCard(t, test.base, -1), markerVisibleCard(t, got, 0)) {
				t.Fatal("adding a marker changed another card field")
			}
		})
	}
}

func TestWithRequestMarkerSkipsOccupiedMarkdown(t *testing.T) {
	const marker = "cabcdefghijklmnopqrs"
	base := `{"schema":"2.0","body":{"elements":[{"tag":"markdown","element_id":"existing","content":"first"},{"tag":"button","text":{"tag":"plain_text","content":"button"}},{"tag":"markdown","content":"second"}]},"extra":{"visible":true}}`

	got, marked, err := WithRequestMarker(base, marker)
	if err != nil || !marked {
		t.Fatalf("WithRequestMarker() = marked %v, err %v", marked, err)
	}
	var card map[string]any
	if err := json.Unmarshal([]byte(got), &card); err != nil {
		t.Fatal(err)
	}
	elements := card["body"].(map[string]any)["elements"].([]any)
	if _, ok := elements[0].(map[string]any)["element_id"]; !ok {
		t.Fatal("existing first element_id was replaced")
	}
	if gotID := elements[2].(map[string]any)["element_id"]; gotID != marker {
		t.Fatalf("second markdown marker = %v, want %q", gotID, marker)
	}
	if !reflect.DeepEqual(markerVisibleCard(t, base, -1), markerVisibleCard(t, got, 2)) {
		t.Fatal("adding a marker changed another card field")
	}
}

func TestWithRequestMarkerRejectsGlobalDuplicate(t *testing.T) {
	const marker = "cnested_marker_12345"
	base := `{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"top level"},{"tag":"column_set","columns":[{"tag":"column","elements":[{"tag":"markdown","element_id":"cnested_marker_12345","content":"nested"}]}]}]}}`

	got, marked, err := WithRequestMarker(base, marker)
	if err == nil || marked || got != "" {
		t.Fatalf("duplicate marker result = %q, marked %v, err %v", got, marked, err)
	}
	if strings.Contains(err.Error(), base) {
		t.Fatal("duplicate error exposed the raw card")
	}
}

func TestWithRequestMarkerRejectsNoFreeMarkdown(t *testing.T) {
	base := `{"schema":"2.0","body":{"elements":[{"tag":"markdown","element_id":"existing","content":"occupied"},{"tag":"button","text":{"tag":"plain_text","content":"button"}}]}}`
	if got, marked, err := WithRequestMarker(base, "cmarker"); err == nil || marked || got != "" {
		t.Fatalf("no-free-markdown result = %q, marked %v, err %v", got, marked, err)
	}
}

func TestWithRequestMarkerRejectsInvalidMarkers(t *testing.T) {
	base := Message("AI", "Codex", "body")
	for _, marker := range []string{"", "1marker", "marker-", "marker space", "émarker", "c" + strings.Repeat("a", 19) + "x"} {
		got, marked, err := WithRequestMarker(base, marker)
		if err == nil || marked || got != "" {
			t.Errorf("marker %q accepted: got %q, marked %v, err %v", marker, got, marked, err)
		}
		if err != nil && strings.Contains(err.Error(), base) {
			t.Errorf("marker %q error exposed the raw card", marker)
		}
	}
}

func TestWithRequestMarkerPreservesLegacyCard(t *testing.T) {
	base := ` { "schema": "1.0", "elements": [{"tag":"markdown","content":"legacy"}] } `
	got, marked, err := WithRequestMarker(base, "cmarker")
	if err != nil || marked || got != base {
		t.Fatalf("legacy result = %q, marked %v, err %v", got, marked, err)
	}
}

func TestWithRequestMarkerRejectsMalformedCard2(t *testing.T) {
	for _, base := range []string{
		"not json",
		`{"schema":"2.0"}`,
		`{"schema":"2.0","body":[]}`,
		`{"schema":"2.0","body":{}}`,
		`{"schema":"2.0","body":{"elements":{}}}`,
	} {
		got, marked, err := WithRequestMarker(base, "cmarker")
		if err == nil || marked || got != "" {
			t.Errorf("malformed card accepted: %q -> %q, marked %v, err %v", base, got, marked, err)
		}
		if strings.Contains(err.Error(), base) {
			t.Errorf("malformed-card error exposed raw input: %v", err)
		}
	}
}

func TestWithRequestMarkerRejectsOversizedCard(t *testing.T) {
	base := Message("AI", "Codex", strings.Repeat("x", DefaultMaxCardBytes))
	got, marked, err := WithRequestMarker(base, "cmarker")
	if err == nil || marked || got != "" {
		t.Fatalf("oversized card accepted: got %q, marked %v, err %v", got, marked, err)
	}
	if strings.Contains(err.Error(), base) {
		t.Fatal("oversized-card error exposed the raw card")
	}
}

func markerVisibleCard(t *testing.T, card string, elementIndex int) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(card), &decoded); err != nil {
		t.Fatal(err)
	}
	if elementIndex >= 0 {
		elements := decoded["body"].(map[string]any)["elements"].([]any)
		delete(elements[elementIndex].(map[string]any), "element_id")
	}
	return decoded
}
