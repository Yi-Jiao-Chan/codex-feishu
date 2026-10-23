package cards

import (
	"encoding/json"
	"errors"
)

// MaxRequestMarkerBytes reserves the serialized field plus a 20-byte ID.
const MaxRequestMarkerBytes = 36

var (
	errInvalidRequestMarker       = errors.New("invalid request marker")
	errInvalidRequestMarkerCard   = errors.New("invalid request marker card")
	errRequestMarkerCardVersion   = errors.New("request marker requires Card JSON 2.0")
	errRequestMarkerCardBody      = errors.New("request marker card requires a body")
	errRequestMarkerCardElements  = errors.New("request marker card requires body elements")
	errRequestMarkerAlreadyExists = errors.New("request marker already exists")
	errRequestMarkerNoMarkdown    = errors.New("request marker card has no free markdown element")
	errRequestMarkerCardTooLarge  = errors.New("request marker card exceeds byte limit")
)

// WithRequestMarker marks a free top-level markdown element, falling back to
// native panel and column bodies. Existing panel IDs remain stable across updates.
// Legacy cards are returned unchanged.
func WithRequestMarker(base, marker string) (rendered string, marked bool, err error) {
	if !validRequestMarker(marker) {
		return "", false, errInvalidRequestMarker
	}

	var card map[string]any
	if err := json.Unmarshal([]byte(base), &card); err != nil || card == nil {
		return "", false, errInvalidRequestMarkerCard
	}

	// Cards without Card JSON 2.0 are not recoverable by this helper. Keep the
	// original bytes so existing legacy-card behavior remains compatible.
	schema, hasSchema := card["schema"]
	if !hasSchema {
		return base, false, nil
	}
	schemaName, ok := schema.(string)
	if !ok {
		return "", false, errRequestMarkerCardVersion
	}
	if schemaName != "2.0" {
		return base, false, nil
	}

	body, ok := card["body"].(map[string]any)
	if !ok || body == nil {
		return "", false, errRequestMarkerCardBody
	}
	elements, ok := body["elements"].([]any)
	if !ok {
		return "", false, errRequestMarkerCardElements
	}
	if requestMarkerExists(card, marker) {
		return "", false, errRequestMarkerAlreadyExists
	}

	if target := freeRequestMarkdown(elements); target != nil {
		target["element_id"] = marker
		encoded, err := json.Marshal(card)
		if err != nil {
			return "", false, errInvalidRequestMarkerCard
		}
		if len(encoded) > DefaultMaxCardBytes {
			return "", false, errRequestMarkerCardTooLarge
		}
		return string(encoded), true, nil
	}

	return "", false, errRequestMarkerNoMarkdown
}

func freeRequestMarkdown(elements []any) map[string]any {
	for _, value := range elements {
		e, ok := value.(map[string]any)
		if !ok || e["tag"] != "markdown" {
			continue
		}
		if _, occupied := e["element_id"]; !occupied {
			return e
		}
	}
	for _, value := range elements {
		e, ok := value.(map[string]any)
		if !ok {
			continue
		}
		switch e["tag"] {
		case "collapsible_panel":
			if children, ok := e["elements"].([]any); ok {
				if target := freeRequestMarkdown(children); target != nil {
					return target
				}
			}
		case "column_set":
			columns, _ := e["columns"].([]any)
			for _, value := range columns {
				column, ok := value.(map[string]any)
				if !ok || column["tag"] != "column" {
					continue
				}
				if children, ok := column["elements"].([]any); ok {
					if target := freeRequestMarkdown(children); target != nil {
						return target
					}
				}
			}
		}
	}
	return nil
}

func validRequestMarker(marker string) bool {
	if len(marker) == 0 || len(marker) > 20 {
		return false
	}
	if !requestMarkerLetter(marker[0]) {
		return false
	}
	for i := 1; i < len(marker); i++ {
		if !requestMarkerLetter(marker[i]) && !requestMarkerDigit(marker[i]) && marker[i] != '_' {
			return false
		}
	}
	return true
}

func requestMarkerLetter(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z'
}

func requestMarkerDigit(char byte) bool {
	return char >= '0' && char <= '9'
}

func requestMarkerExists(value any, marker string) bool {
	switch value := value.(type) {
	case map[string]any:
		if existing, ok := value["element_id"].(string); ok && existing == marker {
			return true
		}
		for _, nested := range value {
			if requestMarkerExists(nested, marker) {
				return true
			}
		}
	case []any:
		for _, nested := range value {
			if requestMarkerExists(nested, marker) {
				return true
			}
		}
	}
	return false
}
