package cards

// SettingsView contains a confirmed local result and its explanatory details.
// Callers decide policy and directory outcomes before rendering the card.
type SettingsView struct {
	Conclusion, Summary, DetailsTitle, Details, Source string
}

func Settings(v SettingsView) string {
	els := []element{{"tag": "markdown", "content": Safe(v.Conclusion), "text_size": "normal"}}
	if v.Summary != "" {
		els = append(els, element{"tag": "markdown", "content": Safe(v.Summary), "text_size": "notation"})
	}
	if v.Details != "" {
		els = append(els, settingsDetails(v.DetailsTitle, v.Details))
	}
	if v.Source != "" {
		els = append(els, footer(v.Source))
	}
	return render(els)
}

func settingsDetails(title, details string) element {
	return element{
		"tag": "collapsible_panel", "expanded": false, "padding": "0px",
		"header": element{
			"title":         element{"tag": "plain_text", "content": Safe(title)},
			"padding":       "0px",
			"icon":          element{"tag": "standard_icon", "token": "down-small-ccm_outlined", "size": "16px 16px"},
			"icon_position": "right",
		},
		"elements": []element{{"tag": "markdown", "content": Safe(details), "text_size": "normal"}},
	}
}
