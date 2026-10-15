package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestPreviewCommandStoreEnforcesPrivateOwnerScope(t *testing.T) {
	for _, variant := range []string{"owner", "group", "other_owner", "nonoperator", "disabled", "missing_secret"} {
		t.Run(variant, func(t *testing.T) {
			s, req, now := commandStoreRequest(t)
			req.Kind, req.OwnerOpenID, req.FilePreviewEnabled, req.PreviewAccessCode = "preview", req.UserID, true, strings.Repeat("fixture-code-", 4)
			switch variant {
			case "group":
				req.ChatType = "group"
			case "other_owner":
				req.OwnerOpenID = "ou_other"
			case "nonoperator":
				req.Operators = nil
			case "disabled":
				req.FilePreviewEnabled = false
			case "missing_secret":
				req.PreviewAccessCode = ""
			}
			r, fresh, err := s.ApplyLocalCommand(context.Background(), req, now)
			if variant != "owner" {
				if !errors.Is(err, ErrConflict) || fresh {
					t.Fatal("store accepted an ineligible credential request")
				}
				return
			}
			if err != nil || !fresh || r.Kind != "preview" || !strings.Contains(r.Card, req.PreviewAccessCode) {
				t.Fatal("private owner reply was not staged", err)
			}
			encoded, err := json.Marshal(req)
			if err != nil || strings.Contains(string(encoded), req.PreviewAccessCode) {
				t.Fatal("command request serialization exposed a credential", err)
			}
		})
	}
}
