package cards

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSettingsKeepsConclusionVisibleAndDetailsNativelyCollapsed(t *testing.T) {
	injected := `<at id="ou_someone"></at>`
	card := Settings(SettingsView{Conclusion: "已临时暂停 " + injected, Summary: "正文继续同步 " + injected, DetailsTitle: "完整设置 " + injected, Details: "时区：Asia/Shanghai\n" + injected, Source: "机器人设置 · 本部署 " + injected})
	var doc map[string]any
	if err := json.Unmarshal([]byte(card), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["schema"] != "2.0" || doc["config"].(map[string]any)["update_multi"] != true {
		t.Fatal("settings lost shared JSON 2.0 configuration")
	}
	els := doc["body"].(map[string]any)["elements"].([]any)
	if len(els) != 4 {
		t.Fatalf("unexpected hierarchy: %d elements", len(els))
	}
	conclusion, summary := els[0].(map[string]any), els[1].(map[string]any)
	if conclusion["text_size"] != "normal" || summary["text_size"] != "notation" || !strings.HasPrefix(conclusion["content"].(string), "已临时暂停") {
		t.Fatal("primary result is not visible before secondary context")
	}
	panel := els[2].(map[string]any)
	if panel["tag"] != "collapsible_panel" || panel["expanded"] != false || panel["padding"] != "0px" {
		t.Fatal("details lost their native collapsed container")
	}
	for _, field := range []string{"border", "background_color"} {
		if _, ok := panel[field]; ok {
			t.Fatalf("details gained %s", field)
		}
	}
	header := panel["header"].(map[string]any)
	if header["title"].(map[string]any)["tag"] != "plain_text" || header["icon"].(map[string]any)["token"] != "down-small-ccm_outlined" || header["padding"] != "0px" {
		t.Fatal("details header is not the native text and disclosure icon")
	}
	if _, ok := header["background_color"]; ok {
		t.Fatal("details header gained a background")
	}
	if _, ok := header["width"]; ok {
		t.Fatal("details gained a newer-client width requirement")
	}
	details := panel["elements"].([]any)[0].(map[string]any)
	if !strings.Contains(details["content"].(string), "时区：Asia/Shanghai\n") {
		t.Fatal("folded content was flattened or lost")
	}
	last := els[3].(map[string]any)
	if last["text_size"] != "notation" || !strings.HasPrefix(last["content"].(string), "> 机器人设置") {
		t.Fatal("source is not the final notation quote")
	}
	for _, text := range []string{conclusion["content"].(string), summary["content"].(string), header["title"].(map[string]any)["content"].(string), details["content"].(string), last["content"].(string)} {
		if strings.Contains(text, "<at") || !strings.Contains(text, "&lt;at") {
			t.Fatal("untrusted settings text introduced a mention")
		}
	}
	if strings.Contains(card, `"tag":"button"`) || strings.Contains(card, `"behaviors"`) {
		t.Fatal("settings introduced an unimplemented action")
	}
}

func TestSettingsOmitsAbsentSecondarySections(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(Settings(SettingsView{Conclusion: "当前没有规则"})), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc["body"].(map[string]any)["elements"].([]any)) != 1 {
		t.Fatal("empty optional sections introduced extra UI")
	}
}
