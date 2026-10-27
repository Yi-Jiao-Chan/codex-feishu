package cards

import "encoding/json"

// WithRetryControl decorates only the first final segment. The caller measures
// the resulting serialized card when choosing message split boundaries.
func WithRetryControl(card, token, state string, revision int64) string {
	var doc struct {
		Schema string         `json:"schema"`
		Config map[string]any `json:"config"`
		Body   map[string]any `json:"body"`
	}
	if json.Unmarshal([]byte(card), &doc) != nil || doc.Body == nil {
		return card
	}
	elements, ok := doc.Body["elements"].([]any)
	if !ok || len(elements) == 0 {
		return card
	}
	last := elements[len(elements)-1]
	elements = elements[:len(elements)-1]
	if state == "available" && token != "" && revision > 0 {
		button := Button("重试", "retry", token)
		button["behaviors"] = []element{{"type": "callback", "value": element{"action": "retry", "token": token, "revision": revision}}}
		elements = append(elements, button)
	} else {
		label := ""
		switch state {
		case "pending":
			label = "已收到 · 正在重试"
		case "unknown":
			label = "正在核验重试结果 · 暂不可操作"
		case "confirmed":
			label = "已提交重试"
		}
		if label == "" {
			return card
		}
		elements = append(elements, element{"tag": "markdown", "content": label, "text_size": "notation"})
	}
	elements = append(elements, last)
	doc.Body["elements"] = elements
	wire, err := json.Marshal(doc)
	if err != nil {
		return card
	}
	return string(wire)
}
