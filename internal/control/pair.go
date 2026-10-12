package control

import (
	"context"
	"time"

	"codex-feishu/internal/config"
	"codex-feishu/internal/pairing"
)

func PairBegin(ctx context.Context, path, identity, requestID string, ttl time.Duration) (pairing.Challenge, error) {
	if ttl < time.Second || ttl > 300*time.Second || ttl%time.Second != 0 {
		return pairing.Challenge{}, pairing.ErrInvalid
	}
	return pairCall(ctx, path, identity, "pair.begin", requestID, int(ttl/time.Second))
}

func PairState(ctx context.Context, path, identity, requestID string) (pairing.Result, error) {
	challenge, err := pairCall(ctx, path, identity, "pair.state", requestID, 0)
	return challenge.Result, err
}

func PairCancel(ctx context.Context, path, identity, requestID string) (pairing.Result, error) {
	challenge, err := pairCall(ctx, path, identity, "pair.cancel", requestID, 0)
	return challenge.Result, err
}

func pairCall(ctx context.Context, path, identity, method, requestID string, ttlSeconds int) (pairing.Challenge, error) {
	result, err := exchange(ctx, path, request{Schema: protocol, Method: method, Identity: identity, Pair: &pairRequest{RequestID: requestID, TTLSeconds: ttlSeconds}})
	if err != nil {
		return pairing.Challenge{}, err
	}
	if result.Pair == nil || result.Snapshot != nil || result.Verification != nil || result.VerificationGroup != nil || result.VerificationThread != nil || result.VerificationBinding != nil || result.Pair.RequestID != requestID || result.Pair.ExpiresAtMS <= 0 {
		return pairing.Challenge{}, ErrUnavailable
	}
	challenge := *result.Pair
	switch challenge.State {
	case "waiting":
		if challenge.OwnerOpenID != "" {
			return pairing.Challenge{}, ErrUnavailable
		}
		if method == "pair.begin" {
			// An idempotent Begin keeps the original deadline, even when a
			// caller retries with a shorter TTL. Bound by the protocol maximum.
			if len(challenge.Code) != 26 || challenge.ExpiresAtMS <= time.Now().UnixMilli() || challenge.ExpiresAtMS > time.Now().Add(301*time.Second).UnixMilli() {
				return pairing.Challenge{}, ErrUnavailable
			}
			for _, c := range challenge.Code {
				if !(c >= 'A' && c <= 'Z' || c >= '2' && c <= '7') {
					return pairing.Challenge{}, ErrUnavailable
				}
			}
		} else if challenge.Code != "" {
			return pairing.Challenge{}, ErrUnavailable
		}
	case "confirmed":
		if !config.ValidOpenID(challenge.OwnerOpenID) || challenge.Code != "" {
			return pairing.Challenge{}, ErrUnavailable
		}
	case "expired", "canceled":
		if challenge.OwnerOpenID != "" || challenge.Code != "" {
			return pairing.Challenge{}, ErrUnavailable
		}
	default:
		return pairing.Challenge{}, ErrUnavailable
	}
	return challenge, nil
}
