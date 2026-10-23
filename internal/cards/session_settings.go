package cards

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxSessionSettingChars = 128

// ModelSettingsText renders the bounded, user-facing session settings source.
// Callers pass the result to the normal card renderer, which applies the
// existing markup escaping and byte budgeting rules.
func ModelSettingsText(model string, effort, tier *string, tierKnown bool) string {
	model = sessionSettingValue(model)
	if model == "" {
		model = "模型未知"
	}
	values := []string{model}
	if effort != nil {
		if effortText := sessionSettingValue(*effort); effortText != "" {
			values = append(values, effortText)
		}
	}
	if tierKnown && tier != nil {
		switch strings.ToLower(sessionSettingValue(*tier)) {
		case "fast", "priority":
			values = append(values, "fast")
		}
	}
	return strings.Join(values, " · ")
}

func sessionSettingValue(value string) string {
	if value == "" || !utf8.ValidString(value) || strings.TrimSpace(value) != value || strings.IndexFunc(value, unicode.IsControl) >= 0 || utf8.RuneCountInString(value) > maxSessionSettingChars {
		return ""
	}
	return value
}
