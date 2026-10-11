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

// WithRequestMarker adds marker to the first top-level markdown element that
// does not already have an element_id. Legacy cards are returned unchanged.
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

	for _, value := range elements {
		element, ok := value.(map[string]any)
		if !ok || element == nil || element["tag"] != "markdown" {
			continue
		}
		if _, exists := element["element_id"]; exists {
			continue
		}
		element["element_id"] = marker
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
