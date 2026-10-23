package cards

import (
	"encoding/json"
	"errors"
)

const (
	ModelSettingsModelField   = "model_settings_model"
	ModelSettingsEffortField  = "model_settings_effort"
	ModelSettingsFastField    = "model_settings_fast"
	ModelSettingsSaveAction   = "model_settings_save"
	ModelSettingsCancelAction = "model_settings_cancel"
)

type ModelSettingsOption struct{ Label, Value string }

// ModelSettingsView contains only display values. The controller validates
// selections against the native catalog again before any settings mutation.
type ModelSettingsView struct {
	State, Current, Notice, Model, Effort, Fast, Token string
	Revision                                           int64
	Models, Efforts                                    []ModelSettingsOption
}

// ModelSettingsCard keeps the editable form and its final result on one card.
// A non-editing state never carries a callback or a form control.
func ModelSettingsCard(v ModelSettingsView) (string, error) {
	text := func(value, size string) element {
		return element{"tag": "markdown", "content": Safe(value), "text_size": size, "margin": "0px"}
	}
	muted := func(value string) element {
		e := text(value, "notation")
		e["content"] = "<font color='grey'>" + Safe(value) + "</font>"
		return e
	}
	var els []element
	if v.State == "editing" {
		if v.Token == "" || v.Revision <= 0 || len(v.Models) == 0 || len(v.Models) > 64 || len(v.Efforts) == 0 || len(v.Efforts) > 32 {
			return "", errors.New("invalid model settings form")
		}
		selectField := func(name, selected, placeholder string, options []ModelSettingsOption) element {
			choices := make([]element, 0, len(options))
			for _, option := range options {
				choices = append(choices, element{"text": element{"tag": "plain_text", "content": option.Label}, "value": option.Value})
			}
			field := element{"tag": "select_static", "name": name, "type": "default", "width": "fill", "options": choices, "placeholder": element{"tag": "plain_text", "content": placeholder}}
			if selected != "" {
				field["initial_option"] = selected
			}
			return field
		}
		button := func(label, action, style string) element {
			b := Button(label, action, v.Token)
			b["type"], b["size"], b["width"] = style, "medium", "default"
			b["name"], b["form_action_type"] = action, "submit"
			b["behaviors"] = []element{{"type": "callback", "value": element{"action": action, "token": v.Token, "revision": v.Revision}}}
			return b
		}
		fast := []ModelSettingsOption{{Label: "关闭", Value: "off"}, {Label: "开启", Value: "on"}}
		if v.Fast == "keep" {
			fast = append([]ModelSettingsOption{{Label: "保持当前", Value: "keep"}}, fast...)
		}
		// This top-level summary also gives request-marker readback an anchor;
		// markers deliberately do not traverse interactive form contents.
		els = append(els, muted("当前："+v.Current))
		fields := []element{
			text("模型", "normal"), selectField(ModelSettingsModelField, v.Model, "选择模型", v.Models),
			{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "16px", "columns": []element{
				{"tag": "column", "width": "weighted", "weight": 1, "elements": []element{text("思考强度", "normal"), selectField(ModelSettingsEffortField, v.Effort, "选择思考强度", v.Efforts)}},
				{"tag": "column", "width": "weighted", "weight": 1, "elements": []element{text("Fast 模式", "normal"), selectField(ModelSettingsFastField, v.Fast, "选择 Fast 模式", fast)}},
			}},
			muted("仅当前会话，从下一轮对话起生效。"),
		}
		if v.Notice != "" {
			fields = append(fields, muted(v.Notice))
		}
		fields = append(fields, creationRow(button("保存", ModelSettingsSaveAction, "primary_filled"), button("取消", ModelSettingsCancelAction, "default")))
		els = append(els, element{"tag": "form", "name": "model_settings_form", "padding": "0px", "margin": "0px", "vertical_spacing": "8px", "elements": fields})
	} else {
		status := map[string]string{
			"loading": "正在读取会话设置", "saving": "正在保存", "canceling": "正在取消",
			"unknown": "正在核验保存结果", "saved": "已保存 · 下一轮生效", "canceled": "已取消", "failed": "未保存",
		}[v.State]
		if status == "" {
			return "", errors.New("invalid model settings card state")
		}
		if v.Current != "" {
			els = append(els, text(v.Current, "normal"))
		}
		els = append(els, muted(status))
		if v.Notice != "" {
			els = append(els, muted(v.Notice))
		}
	}
	card := element{
		"schema": "2.0",
		"config": element{"update_multi": true, "width_mode": "compact", "enable_forward": false, "enable_forward_interaction": false},
		"body":   element{"elements": els, "padding": "12px 16px", "vertical_spacing": "8px"},
	}
	if v.State == "editing" {
		card["header"] = element{"title": element{"tag": "plain_text", "content": "模型设置"}, "template": "blue"}
	}
	raw, err := json.Marshal(card)
	if err != nil || len(raw) > DefaultMaxCardBytes-512 {
		return "", errors.New("model settings card exceeds display limit")
	}
	return string(raw), nil
}
