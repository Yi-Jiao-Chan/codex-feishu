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
	// Recovery reads share a total budget, rather than paying the transport
	// timeout on every history page and monopolizing a thread actor.
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	a := r.Attempt
	var msg feishu.CardMessage
	if a.Operation == "update" {
		msg, err = o.Readback.ReadCardMessage(checkCtx, a.TargetMessageID)
		if err != nil {
			return DeliveryResult{}, err
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
			page, e := o.Readback.ListCardMessages(checkCtx, "chat", a.ChatID, cursor)
			if e != nil {
				return DeliveryResult{}, e
			}
			if len(page.Items) > 50 {
				return DeliveryResult{}, ErrReadbackIncomplete
			}
			for _, item := range page.Items {
				if item.Type != "interactive" {
					continue
				}
				count := markerCount(item.Content, r.Marker)
				if count == 0 {
					continue
				}
				if item.ID == "" || count != 1 {
					return DeliveryResult{}, ErrReadbackIncomplete
				}
				if previous, ok := found[item.ID]; ok && !reflect.DeepEqual(previous, item) {
					return DeliveryResult{}, ErrReadbackIncomplete
				}
				found[item.ID] = item
			}
			if len(found) > 1 {
				return DeliveryResult{}, ErrReadbackIncomplete
			}
			if !page.HasMore {
				complete = true
				break
			}
			if page.Token == "" || seenTokens[page.Token] {
				return DeliveryResult{}, ErrReadbackIncomplete
			}
			seenTokens[page.Token] = true
			cursor = page.Token
		}
		if !complete {
			return DeliveryResult{}, ErrReadbackIncomplete
		}
		if len(found) == 0 {
			return DeliveryResult{}, nil
		}
		for id := range found {
			// Re-read the unique candidate after complete pagination so a stale
			// list representation cannot confirm a subsequently changed card.
			msg, err = o.Readback.ReadCardMessage(checkCtx, id)
			if err != nil {
				return DeliveryResult{}, err
			}
			if msg.ID != id {
				return DeliveryResult{}, ErrReadbackIncomplete
			}
		}
	}
	if err := checkCtx.Err(); err != nil {
		return DeliveryResult{}, err
	}
	if !matchesReadback(r, msg) {
		return DeliveryResult{}, nil
	}
	if err := o.Store.FinishMirrorAttempt(ctx, a.ID, "confirmed", msg.ID); err != nil {
		return DeliveryResult{}, err
	}
	m, err := o.Store.Mirror(ctx, host, thread, a.Key)
	if err != nil {
		return DeliveryResult{}, err
	}
	return DeliveryResult{AttemptID: a.ID, Key: a.Key, Kind: m.Kind, TurnID: m.TurnID, State: "confirmed", MessageID: msg.ID}, nil
}

func matchesReadback(r store.MirrorReadback, m feishu.CardMessage) bool {
	a := r.Attempt
	if m.ID == "" || m.ChatID != a.ChatID || m.Type != "interactive" || m.SenderType != "app" || m.SenderIDType != "app_id" || m.SenderID != r.AppID || m.Deleted == nil || *m.Deleted {
		return false
	}
	if a.Operation == "update" && m.ID != a.TargetMessageID {
		return false
	}
	if m.ThreadID != nil && *m.ThreadID != "" {
		return false
	}
	if a.ParentMessageID != "" {
		if m.ParentID == nil || *m.ParentID != a.ParentMessageID {
			return false
		}
	} else if m.ParentID != nil && *m.ParentID != "" || m.RootID != nil && *m.RootID != "" {
		return false
	}
	expected, err := parseProofJSON(a.Card)
	if err != nil {
		return false
	}
	actual, err := parseProofJSON(m.Content)
	return err == nil && markerCountValue(expected, r.Marker) == 1 && reflect.DeepEqual(expected, actual)
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
