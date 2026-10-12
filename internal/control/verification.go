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
	if r.Verification == nil || r.Snapshot != nil || r.Pair != nil || r.VerificationGroup != nil || r.Verification.RequestID != id || r.Verification.BridgeCommit == "" || !verification.ValidBinaryHash(r.Verification.BinarySHA256) || r.Verification.ExpiresAtMS <= 0 {
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

func VerifyGroup(ctx context.Context, path, identity, id string, begin bool) (verification.GroupResult, error) {
	if !verification.ValidRequestID(id) {
		return verification.GroupResult{}, verification.ErrInvalid
	}
	method := "verify.group.state"
	if begin {
		method = "verify.group.begin"
	}
	r, err := exchange(ctx, path, request{Schema: protocol, Method: method, Identity: identity, VerificationGroup: &verificationRequest{RequestID: id}})
	if err != nil {
		return verification.GroupResult{}, err
	}
	if r.VerificationGroup == nil || r.Snapshot != nil || r.Pair != nil || r.Verification != nil || r.VerificationGroup.RequestID != id || r.VerificationGroup.BridgeCommit == "" || !verification.ValidBinaryHash(r.VerificationGroup.BinarySHA256) || r.VerificationGroup.ExpiresAtMS <= 0 {
		return verification.GroupResult{}, ErrUnavailable
	}
	v := *r.VerificationGroup
	if v.State != "ready" && v.State != "unknown" && v.State != "confirmed" && v.State != "rejected" {
		return verification.GroupResult{}, ErrUnavailable
	}
	if v.GroupStatus != "pass" && v.GroupStatus != "fail" && v.GroupStatus != "pending" || v.MembershipStatus != "pass" && v.MembershipStatus != "pending" || v.GroupMessageStatus != "pass" && v.GroupMessageStatus != "pending" {
		return verification.GroupResult{}, ErrUnavailable
	}
	if v.GroupStatus == "pass" && (v.State != "confirmed" || v.ChatID == "") || v.GroupStatus == "fail" && v.State != "rejected" || v.MembershipStatus == "pass" && v.GroupStatus != "pass" || v.GroupMessageStatus == "pass" && v.MembershipStatus != "pass" {
		return verification.GroupResult{}, ErrUnavailable
	}
	return v, nil
}
