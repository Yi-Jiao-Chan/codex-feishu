package control

import (
	"codex-feishu/internal/verification"
	"context"
)

func VerifyCard(ctx context.Context, path, identity, id string, begin bool) (verification.Result, error) {
	if !verification.ValidRequestID(id) {
		return verification.Result{}, verification.ErrInvalid
	}
	method := "verify.card.state"
	if begin {
		method = "verify.card.begin"
	}
	r, err := exchange(ctx, path, request{Schema: protocol, Method: method, Identity: identity, Verification: &verificationRequest{RequestID: id}})
	if err != nil {
		return verification.Result{}, err
	}
	if r.Verification == nil || r.Snapshot != nil || r.Pair != nil || r.Verification.RequestID != id || r.Verification.BridgeCommit == "" || !verification.ValidBinaryHash(r.Verification.BinarySHA256) || r.Verification.ExpiresAtMS <= 0 {
		return verification.Result{}, ErrUnavailable
	}
	v := *r.Verification
	if v.State != "ready" && v.State != "unknown" && v.State != "confirmed" && v.State != "rejected" {
		return verification.Result{}, ErrUnavailable
	}
	if v.CardStatus != "pass" && v.CardStatus != "fail" && v.CardStatus != "pending" || v.CallbackStatus != "pass" && v.CallbackStatus != "pending" || v.PrivateMessageStatus != "pass" && v.PrivateMessageStatus != "pending" {
		return verification.Result{}, ErrUnavailable
	}
	if v.CardStatus == "pass" && (v.State != "confirmed" || v.MessageID == "") || v.CardStatus == "fail" && v.State != "rejected" || (v.CallbackStatus == "pass" || v.PrivateMessageStatus == "pass") && v.CardStatus != "pass" {
		return verification.Result{}, ErrUnavailable
	}
	return v, nil
}
