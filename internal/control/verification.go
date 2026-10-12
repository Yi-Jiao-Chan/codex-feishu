package control

import (
	"codex-feishu/internal/verification"
	"context"
	"time"
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
	if r.Verification == nil || r.Snapshot != nil || r.Pair != nil || r.VerificationGroup != nil || r.VerificationThread != nil || r.VerificationBinding != nil || r.Verification.RequestID != id || r.Verification.BridgeCommit == "" || !verification.ValidBinaryHash(r.Verification.BinarySHA256) || r.Verification.ExpiresAtMS <= 0 {
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
	if r.VerificationGroup == nil || r.Snapshot != nil || r.Pair != nil || r.Verification != nil || r.VerificationThread != nil || r.VerificationBinding != nil || r.VerificationGroup.RequestID != id || r.VerificationGroup.BridgeCommit == "" || !verification.ValidBinaryHash(r.VerificationGroup.BinarySHA256) || r.VerificationGroup.ExpiresAtMS <= 0 {
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

func VerifyThreadCreation(ctx context.Context, path, identity, id string, begin bool) (verification.NativeSessionResult, error) {
	if !verification.ValidRequestID(id) {
		return verification.NativeSessionResult{}, verification.ErrInvalid
	}
	method := "verify.thread.state"
	if begin {
		method = "verify.thread.begin"
	}
	r, err := exchange(ctx, path, request{Schema: protocol, Method: method, Identity: identity, VerificationThread: &verificationRequest{RequestID: id}})
	if err != nil {
		return verification.NativeSessionResult{}, err
	}
	if r.VerificationThread == nil || r.Snapshot != nil || r.Pair != nil || r.Verification != nil || r.VerificationGroup != nil || r.VerificationBinding != nil || r.VerificationThread.RequestID != id || r.VerificationThread.BridgeCommit == "" || !verification.ValidBinaryHash(r.VerificationThread.BinarySHA256) || r.VerificationThread.ExpiresAtMS <= 0 {
		return verification.NativeSessionResult{}, ErrUnavailable
	}
	v := *r.VerificationThread
	if v.ModelRequests < 0 || v.ResourcesReady == v.ResourceRecoveryRequired || v.CreatorConnected && v.CreatorGeneration <= 0 {
		return verification.NativeSessionResult{}, ErrUnavailable
	}
	switch v.State {
	case "ready":
		if v.CreationStatus != "pending" || v.CreatorGeneration != 0 || v.ThreadID != "" {
			return verification.NativeSessionResult{}, ErrUnavailable
		}
	case "unknown":
		if v.CreationStatus != "pending" || v.CreatorGeneration <= 0 || v.ThreadID != "" {
			return verification.NativeSessionResult{}, ErrUnavailable
		}
	case "confirmed":
		if v.CreationStatus != "pass" || v.CreatorGeneration <= 0 || v.ThreadID == "" {
			return verification.NativeSessionResult{}, ErrUnavailable
		}
	case "rejected":
		if v.CreationStatus != "fail" || v.CreatorGeneration <= 0 || v.ThreadID != "" {
			return verification.NativeSessionResult{}, ErrUnavailable
		}
	default:
		return verification.NativeSessionResult{}, ErrUnavailable
	}
	return v, nil
}

func VerifyBinding(ctx context.Context, path, identity, id string, begin bool) (verification.BindingResult, error) {
	if !verification.ValidRequestID(id) {
		return verification.BindingResult{}, verification.ErrInvalid
	}
	method := "verify.binding.state"
	if begin {
		method = "verify.binding.begin"
	}
	r, err := exchange(ctx, path, request{Schema: protocol, Method: method, Identity: identity, VerificationBinding: &verificationRequest{RequestID: id}})
	if err != nil {
		return verification.BindingResult{}, err
	}
	if r.VerificationBinding == nil || r.Snapshot != nil || r.Pair != nil || r.Verification != nil || r.VerificationGroup != nil || r.VerificationThread != nil || r.VerificationBinding.RequestID != id {
		return verification.BindingResult{}, ErrUnavailable
	}
	v := *r.VerificationBinding
	if err := validateBindingResult(v, id, begin); err != nil {
		return verification.BindingResult{}, ErrUnavailable
	}
	return v, nil
}

func validateBindingResult(v verification.BindingResult, id string, begin bool) error {
	if v.RequestID != id || v.BridgeCommit == "" || !verification.ValidBinaryHash(v.BinarySHA256) || v.ExpiresAtMS <= 0 || v.BoundAtMS <= 0 || v.ExpiresAtMS <= v.BoundAtMS {
		return ErrUnavailable
	}
	if v.BindingStatus != "pass" && v.BindingStatus != "pending" {
		return ErrUnavailable
	}
	if v.BindingStatus == "pass" && (v.ThreadID == "" || v.ChatID == "") {
		return ErrUnavailable
	}
	if v.BindingStatus == "pending" && (v.ActorStarted || v.ActivationRequested) {
		return ErrUnavailable
	}
	if !begin && v.ActivationRequested {
		return ErrUnavailable
	}
	if v.Expired != (time.Now().UnixMilli() >= v.ExpiresAtMS) {
		return ErrUnavailable
	}
	if (v.ActorStarted || v.ActivationRequested) && (v.BuildChanged || v.Expired || !v.ResourcesReady || !v.CreatorConnected) {
		return ErrUnavailable
	}
	if err := validateInputEvidence(v); err != nil {
		return err
	}
	return validateDeliveryEvidence(v)
}

// Missing fields from an older provider remain missing evidence, never zero
// observations reported as a successful snapshot.
func validateInputEvidence(v verification.BindingResult) error {
	e := v.InputEvidence
	if e == nil {
		if v.InputEvidenceStatus == "" || v.InputEvidenceStatus == "pending" {
			return nil
		}
		return ErrUnavailable
	}
	if v.InputEvidenceStatus != "pass" || v.BindingStatus != "pass" || e.StagedInputs < 0 || e.AddConfirmed < 0 || e.QueuedSnapshot < 0 || e.HistoryLinked < 0 || e.SteerConfirmed < 0 || e.SteerHistoryLinked < 0 || e.UnresolvedAttempts < 0 || e.AddConfirmed > e.StagedInputs || e.QueuedSnapshot > e.StagedInputs || e.HistoryLinked > e.StagedInputs || e.SteerConfirmed > e.StagedInputs || e.SteerHistoryLinked > e.SteerConfirmed || e.SteerHistoryLinked > e.HistoryLinked {
		return ErrUnavailable
	}
	return nil
}

func validateDeliveryEvidence(v verification.BindingResult) error {
	e := v.DeliveryEvidence
	if e == nil {
		if v.DeliveryEvidenceStatus == "" || v.DeliveryEvidenceStatus == "pending" {
			return nil
		}
		return ErrUnavailable
	}
	if v.DeliveryEvidenceStatus != "pass" || v.BindingStatus != "pass" || e.ProgressCards < 0 || e.ProgressConfirmed < 0 || e.FinalCards < 0 || e.FinalConfirmed < 0 || e.FinalTurns < 0 || e.FinalTurnsDelivered < 0 || e.ReferencedCards < 0 || e.UnresolvedAttempts < 0 || e.ProgressConfirmed > e.ProgressCards || e.FinalConfirmed > e.FinalCards || e.FinalTurns > e.FinalCards || e.FinalTurnsDelivered > e.FinalTurns || e.FinalTurnsDelivered > e.FinalConfirmed || e.ReferencedCards > e.ProgressCards+e.FinalCards || e.ProgressConfirmed+e.FinalConfirmed > e.ReferencedCards {
		return ErrUnavailable
	}
	return nil
}

func VerifyTurnRelease(ctx context.Context, path, identity, id string) (verification.BindingResult, error) {
	if !verification.ValidRequestID(id) {
		return verification.BindingResult{}, verification.ErrInvalid
	}
	r, err := exchange(ctx, path, request{Schema: protocol, Method: "verify.binding.release", Identity: identity, VerificationBinding: &verificationRequest{RequestID: id}})
	if err != nil {
		return verification.BindingResult{}, err
	}
	if r.VerificationBinding == nil || r.Snapshot != nil || r.Pair != nil || r.Verification != nil || r.VerificationGroup != nil || r.VerificationThread != nil || r.VerificationBinding.RequestID != id {
		return verification.BindingResult{}, ErrUnavailable
	}
	v := *r.VerificationBinding
	if err := validateBindingResult(v, id, false); err != nil || v.ModelReleased != true || v.ActivationRequested || v.BindingStatus != "pass" || v.BuildChanged || v.Expired || !v.ResourcesReady || !v.CreatorConnected {
		return verification.BindingResult{}, ErrUnavailable
	}
	return v, nil
}
