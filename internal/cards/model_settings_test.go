package cards

import (
	"encoding/json"
	"strings"
	"testing"
)

func modelSettingsCardFixture() ModelSettingsView {
	return ModelSettingsView{State: "editing", Current: "fixture-model · high", Model: "fixture-model", Effort: "high", Fast: "off", Token: "test-token", Revision: 2,
		Models: []ModelSettingsOption{{Label: "fixture-model", Value: "fixture-model"}}, Efforts: []ModelSettingsOption{{Label: "high", Value: "high"}}}
}

func TestModelSettingsFormCarriesSelectionsAndBothExplicitActions(t *testing.T) {
	v := modelSettingsCardFixture()
	raw, err := ModelSettingsCard(v)
	if err != nil {
		t.Fatal(err)
	}
	var card map[string]any
	if err := json.Unmarshal([]byte(raw), &card); err != nil {
		t.Fatal(err)
	}
	config := card["config"].(map[string]any)
	if config["update_multi"] != true || config["enable_forward"] != false || config["enable_forward_interaction"] != false {
		t.Fatal("form may be forwarded or lacks shared updates")
	}
	fields, actions, forms := map[string]string{}, map[string]bool{}, 0
	walkInteractionJSON(card, func(node map[string]any) {
		switch node["tag"] {
		case "form":
			forms++
		case "select_static":
			if node["required"] == true || node["behaviors"] != nil {
				t.Fatal("draft edits must not submit or block cancel")
			}
			fields[node["name"].(string)], _ = node["initial_option"].(string)
		case "button":
			if node["form_action_type"] != "submit" {
				t.Fatal("settings action does not submit form values")
			}
			behaviors := node["behaviors"].([]any)
			value := behaviors[0].(map[string]any)["value"].(map[string]any)
			if value["token"] != v.Token || value["revision"] != float64(v.Revision) {
				t.Fatal("button lost action scope")
			}
			actions[value["action"].(string)] = true
		}
	})
	if forms != 1 || len(fields) != 3 || fields[ModelSettingsModelField] != "fixture-model" || fields[ModelSettingsEffortField] != "high" || fields[ModelSettingsFastField] != "off" || len(actions) != 2 || !actions[ModelSettingsSaveAction] || !actions[ModelSettingsCancelAction] {
		t.Fatal("form omitted current choices or save/cancel")
	}
	marked, ok, err := WithRequestMarker(raw, "settings_marker")
	if err != nil || !ok || !strings.Contains(marked, "settings_marker") {
		t.Fatal("form cannot reconcile a lost send/PATCH", err)
	}
}

func TestModelSettingsTerminalCardsAreCompactAndNonInteractive(t *testing.T) {
	for _, state := range []string{"saved", "canceled", "failed", "unknown"} {
		raw, err := ModelSettingsCard(ModelSettingsView{State: state, Current: "fixture-model · max · fast", Token: "must-not-render", Revision: 9})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(raw, "fixture-model · max · fast") || strings.Contains(raw, "must-not-render") || strings.Contains(raw, "header") || strings.Contains(raw, "behaviors") || strings.Contains(raw, "select_static") {
			t.Fatal("terminal retained editing controls or redundant heading")
		}
		if _, marked, err := WithRequestMarker(raw, "terminal_marker"); err != nil || !marked {
			t.Fatal("terminal update has no delivery evidence anchor", err)
		}
	}
}

func TestModelSettingsFormFailsClosedWhenCatalogCannotFit(t *testing.T) {
	v := modelSettingsCardFixture()
	v.Models = make([]ModelSettingsOption, 65)
	if _, err := ModelSettingsCard(v); err == nil {
		t.Fatal("catalog was silently truncated")
	}
	v = modelSettingsCardFixture()
	v.Models[0].Label = strings.Repeat("large", DefaultMaxCardBytes)
	if _, err := ModelSettingsCard(v); err == nil {
		t.Fatal("oversized form exceeded card wire budget")
	}
}
