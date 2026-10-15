package sync

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"time"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type MessageReadbackGateway interface {
	ReadCardMessage(context.Context, string) (feishu.CardMessage, error)
	ListCardMessages(context.Context, string, string, string) (feishu.CardMessagePage, error)
}

type CardReadbackProof struct {
	// RootCreation is reserved for a SendCard that created an original root.
	// Later replies may assign that same root a platform thread identity.
	RootCreation                                bool   `json:"-"`
	FollowMode, RootMessageID, PlatformThreadID string `json:"-"`
	AppID                                       string `json:"-"`
	ChatID                                      string `json:"-"`
	ParentMessageID                             string `json:"-"`
	TargetMessageID                             string `json:"-"`
	Card                                        string `json:"-"`
	Marker                                      string `json:"-"`
}

var ErrReadbackIncomplete = errors.New("message readback evidence incomplete")

// ReconcileOne performs GETs only. Only a pre-send, globally reserved marker
// plus the full frozen document and identity/relationship can confirm. Failure
// or absence never rejects/reopens an unknown attempt or resends its payload.
func (o Outbound) ReconcileOne(ctx context.Context, host, thread string, nowMS int64) (DeliveryResult, error) {
	if o.Readback == nil || o.Store == nil {
		return DeliveryResult{}, nil
	}
	r, claimed, err := o.Store.ClaimMirrorReadback(ctx, host, thread, nowMS, 30000)
	if err != nil || !claimed {
		return DeliveryResult{}, err
	}
	a := r.Attempt
	messageID, err := FindMarkedCard(ctx, o.Readback, CardReadbackProof{
		FollowMode: a.FollowMode, RootMessageID: a.RootMessageID, PlatformThreadID: a.PlatformThreadID,
		AppID:           r.AppID,
		ChatID:          a.ChatID,
		ParentMessageID: a.ParentMessageID,
		TargetMessageID: a.TargetMessageID,
		Card:            a.Card,
		Marker:          r.Marker,
	})
	if err != nil || messageID == "" {
		return DeliveryResult{}, err
	}
	if err := o.Store.FinishMirrorAttempt(ctx, a.ID, "confirmed", messageID); err != nil {
		return DeliveryResult{}, err
	}
	m, err := o.Store.Mirror(ctx, host, thread, a.Key)
	if err != nil {
		return DeliveryResult{}, err
	}
	return DeliveryResult{AttemptID: a.ID, Key: a.Key, Kind: m.Kind, TurnID: m.TurnID, State: "confirmed", MessageID: messageID}, nil
}

// FindMarkedCard performs a GET-only proof of one pre-send marker. A blank
// result means the available readback is inconclusive; it never means that a
// remote write was rejected.
func FindMarkedCard(ctx context.Context, reader MessageReadbackGateway, proof CardReadbackProof) (string, error) {
	if reader == nil || proof.AppID == "" || proof.ChatID == "" || proof.Card == "" || proof.Marker == "" || proof.RootCreation && proof.ParentMessageID != "" {
		return "", ErrReadbackIncomplete
	}
	// Recovery reads share a total budget, rather than paying the transport
	// timeout on every history page and monopolizing a thread actor.
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var msg feishu.CardMessage
	var err error
	if proof.TargetMessageID != "" && !proof.RootCreation {
		msg, err = reader.ReadCardMessage(checkCtx, proof.TargetMessageID)
		if err != nil {
			return "", err
		}
	} else {
		// Phase1 uses ordinary group replies, explicitly reply_in_thread=false.
		// A bounded scan that stops early is inconclusive even if it found a
		// candidate. Do not select the first copy of a marker.
		cursor := ""
		seenTokens := map[string]bool{"": true}
		found := map[string]feishu.CardMessage{}
		complete := false
		for n := 0; n < 20; n++ {
			containerType, containerID := "chat", proof.ChatID
			if !proof.RootCreation && proof.FollowMode == "topic" && proof.PlatformThreadID != "" {
				containerType, containerID = "thread", proof.PlatformThreadID
			}
			page, e := reader.ListCardMessages(checkCtx, containerType, containerID, cursor)
			if e != nil {
				return "", e
			}
			if len(page.Items) > 50 {
				return "", ErrReadbackIncomplete
			}
			for _, item := range page.Items {
				if item.Type != "interactive" {
					continue
				}
				count := markerCount(item.Content, proof.Marker)
				if count == 0 {
					continue
				}
				if item.ID == "" || count != 1 {
					return "", ErrReadbackIncomplete
				}
				if previous, ok := found[item.ID]; ok && !reflect.DeepEqual(previous, item) {
					return "", ErrReadbackIncomplete
				}
				found[item.ID] = item
			}
			if len(found) > 1 {
				return "", ErrReadbackIncomplete
			}
			if !page.HasMore {
				complete = true
				break
			}
			if page.Token == "" || seenTokens[page.Token] {
				return "", ErrReadbackIncomplete
			}
			seenTokens[page.Token] = true
			cursor = page.Token
		}
		if !complete {
			return "", ErrReadbackIncomplete
		}
		if len(found) == 0 {
			return "", nil
		}
		for id := range found {
			// Re-read the unique candidate after complete pagination so a stale
			// list representation cannot confirm a subsequently changed card.
			msg, err = reader.ReadCardMessage(checkCtx, id)
			if err != nil {
				return "", err
			}
			if msg.ID != id {
				return "", ErrReadbackIncomplete
			}
		}
	}
	if err := checkCtx.Err(); err != nil {
		return "", err
	}
	if !matchesCardReadback(proof, msg) {
		return "", nil
	}
	return msg.ID, nil
}

// matchesReadback keeps the store-shaped helper available to existing
// package-local callers while proof-based callers use matchesCardReadback.
func matchesReadback(r store.MirrorReadback, m feishu.CardMessage) bool {
	a := r.Attempt
	return matchesCardReadback(CardReadbackProof{
		FollowMode: a.FollowMode, RootMessageID: a.RootMessageID, PlatformThreadID: a.PlatformThreadID,
		AppID:           r.AppID,
		ChatID:          a.ChatID,
		ParentMessageID: a.ParentMessageID,
		TargetMessageID: a.TargetMessageID,
		Card:            a.Card,
		Marker:          r.Marker,
	}, m)
}

func matchesCardReadback(proof CardReadbackProof, m feishu.CardMessage) bool {
	if m.ID == "" || m.ChatID != proof.ChatID || m.Type != "interactive" || m.SenderType != "app" || m.SenderIDType != "app_id" || m.SenderID != proof.AppID || m.Deleted == nil || *m.Deleted {
		return false
	}
	if proof.TargetMessageID != "" && m.ID != proof.TargetMessageID {
		return false
	}
	if proof.RootCreation {
		if proof.ParentMessageID != "" || m.ParentID != nil && *m.ParentID != "" {
			return false
		}
		if m.RootID != nil && *m.RootID != "" && *m.RootID != m.ID {
			return false
		}
		if proof.RootMessageID != "" && proof.RootMessageID != m.ID {
			return false
		}
		if proof.PlatformThreadID != "" && (m.ThreadID == nil || *m.ThreadID != proof.PlatformThreadID) {
			return false
		}
	} else {
		if proof.FollowMode == "topic" {
			if proof.RootMessageID == "" && proof.PlatformThreadID == "" || m.RootID == nil || *m.RootID == "" || m.ThreadID == nil || *m.ThreadID == "" {
				return false
			}
			if proof.RootMessageID != "" && *m.RootID != proof.RootMessageID {
				return false
			}
			if proof.PlatformThreadID != "" && *m.ThreadID != proof.PlatformThreadID {
				return false
			}
		} else if m.ThreadID != nil && *m.ThreadID != "" {
			return false
		}
		if proof.ParentMessageID != "" {
			if m.ParentID == nil || *m.ParentID != proof.ParentMessageID {
				return false
			}
		} else if m.ParentID != nil && *m.ParentID != "" || m.RootID != nil && *m.RootID != "" {
			return false
		}
	}

	expected, err := parseProofJSON(proof.Card)
	if err != nil {
		return false
	}
	actual, err := parseProofJSON(m.Content)
	return err == nil && markerCountValue(expected, proof.Marker) == 1 && reflect.DeepEqual(expected, actual)
}

func markerCount(card, marker string) int {
	if marker == "" {
		return 0
	}
	v, err := parseProofJSON(card)
	if err != nil {
		return -1 // Unparseable evidence cannot prove absence of this marker.
	}
	return markerCountValue(v, marker)
}

func markerCountValue(v any, marker string) int {
	count := 0
	switch v := v.(type) {
	case map[string]any:
		if v["element_id"] == marker {
			count++
		}
		for _, child := range v {
			count += markerCountValue(child, marker)
		}
	case []any:
		for _, child := range v {
			count += markerCountValue(child, marker)
		}
	}
	return count
}

// Reject duplicate keys, extra JSON values, deep documents and type changes.
// Never normalize away controls, mentions or fields that look like defaults.
func parseProofJSON(raw string) (any, error) {
	if len(raw) > 1<<20 {
		return nil, ErrReadbackIncomplete
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	budget := 10000
	v, err := proofValue(d, 0, &budget)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrReadbackIncomplete
	}
	return v, nil
}

func proofValue(d *json.Decoder, depth int, budget *int) (any, error) {
	*budget--
	if depth > 64 || *budget < 0 {
		return nil, ErrReadbackIncomplete
	}
	t, err := d.Token()
	if err != nil {
		return nil, ErrReadbackIncomplete
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return t, nil
	}
	switch delim {
	case '{':
		v := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, ErrReadbackIncomplete
			}
			s, ok := key.(string)
			if !ok {
				return nil, ErrReadbackIncomplete
			}
			if _, exists := v[s]; exists {
				return nil, ErrReadbackIncomplete
			}
			child, err := proofValue(d, depth+1, budget)
			if err != nil {
				return nil, err
			}
			v[s] = child
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, ErrReadbackIncomplete
		}
		return v, nil
	case '[':
		v := []any{}
		for d.More() {
			child, err := proofValue(d, depth+1, budget)
			if err != nil {
				return nil, err
			}
			v = append(v, child)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, ErrReadbackIncomplete
		}
		return v, nil
	default:
		return nil, ErrReadbackIncomplete
	}
}
