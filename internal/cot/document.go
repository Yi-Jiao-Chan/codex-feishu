// Package cot projects already-public progress into bounded native COT events.
// It does not inspect native tool inputs, perform I/O, or advance delivery state.
package cot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

const Version = 1

// Document is a complete display-safe snapshot, not an append request. The
// distinct marker prevents an ordinary Card JSON payload being sent as COT.
type Document struct {
	Version  int    `json:"codex_feishu_cot"`
	ThreadID string `json:"thread_id"`
	TurnID   string `json:"turn_id"`
	Status   string `json:"status"`
	Items    []Item `json:"items"`
}

// Item contains only public text or an existing safe tool summary. TimestampMS
// is first local observation, not a native invocation or generation timestamp.
type Item struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Text        string `json:"text,omitempty"`
	Title       string `json:"title,omitempty"`
	Status      string `json:"status,omitempty"`
	Icon        string `json:"icon,omitempty"`
	TimestampMS int64  `json:"timestamp_ms"`
}

// Parse returns recognized=true even for an invalid/future COT document, so a
// malformed native document cannot accidentally fall back to Card JSON delivery.
func Parse(raw string) (doc Document, recognized bool, err error) {
	var header map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &header) != nil {
		return doc, false, nil
	}
	if _, recognized = header["codex_feishu_cot"]; !recognized {
		return doc, false, nil
	}
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return doc, true, errors.New("invalid COT document")
	}
	return doc, true, validateDocument(doc)
}

func Encode(doc Document) (string, error) {
	if err := validateDocument(doc); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(doc)
	return string(encoded), err
}

func validateDocument(doc Document) error {
	if doc.Version != Version || !validID(doc.ThreadID) || !validID(doc.TurnID) ||
		(doc.Status != "inProgress" && doc.Status != "completed" && doc.Status != "failed" && doc.Status != "interrupted") {
		return errors.New("invalid COT document scope or status")
	}
	seen := make(map[string]bool, len(doc.Items))
	for _, item := range doc.Items {
		if !validID(item.ID) || seen[item.ID] || item.TimestampMS <= 0 || !utf8.ValidString(item.Text) || !utf8.ValidString(item.Title) {
			return errors.New("invalid or duplicate COT item")
		}
		seen[item.ID] = true
		switch item.Kind {
		case "message":
			if item.Title != "" || item.Status != "" || item.Icon != "" {
				return errors.New("invalid public COT message metadata")
			}
		case "tool":
			if item.Text != "" || strings.TrimSpace(item.Title) == "" ||
				utf8.RuneCountInString(item.Title) > 120 || strings.ContainsAny(item.Title, "\r\n") ||
				(item.Status != "inProgress" && item.Status != "completed" && item.Status != "failed" && item.Status != "declined" && item.Status != "unknown") {
				return errors.New("invalid public COT tool metadata")
			}
			switch item.Icon {
			case "", "bash", "read", "write", "search", "task", "default":
			default:
				return errors.New("invalid public COT tool icon")
			}
		default:
			return errors.New("invalid COT item kind")
		}
	}
	return nil
}

func validID(id string) bool {
	return id != "" && len(id) <= 1024 && utf8.ValidString(id) && !strings.ContainsAny(id, "\x00\r\n")
}

// StableID scopes all public event identities to a turn. Native item IDs are
// hashed so they cannot accidentally expose a native path or escape a field.
func StableID(doc Document, kind, item string, generation int) string {
	encoded, _ := json.Marshal([]any{doc.ThreadID, doc.TurnID, kind, item, generation})
	sum := sha256.Sum256(encoded)
	return "cot_" + hex.EncodeToString(sum[:16])
}
