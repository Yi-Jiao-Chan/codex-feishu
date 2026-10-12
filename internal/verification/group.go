package verification

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

var ErrNotFound = errors.New("verification request not found")

type GroupGateway interface {
	CreateGroup(context.Context, string, string, []string, string) (string, error)
	BotInChat(context.Context, string) (bool, error)
	MemberIDs(context.Context, string) ([]string, error)
}

type GroupResult struct {
	RequestID          string `json:"request_id"`
	BridgeCommit       string `json:"bridge_commit"`
	BinarySHA256       string `json:"binary_sha256"`
	BuildChanged       bool   `json:"build_changed"`
	State              string `json:"state"`
	GroupStatus        string `json:"group_status"`
	MembershipStatus   string `json:"membership_status"`
	GroupMessageStatus string `json:"group_message_status"`
	ChatID             string `json:"chat_id,omitempty"`
	ExpiresAtMS        int64  `json:"expires_at_ms"`
	Expired            bool   `json:"expired"`
}

// GroupManager only creates an owner-and-bot test group. It neither binds a
// native thread nor sends cards/messages; group authorization grants no input.
type GroupManager struct {
	Store                                               *store.Store
	Gateway                                             GroupGateway
	HostID, AppID, OwnerID, BotID, Commit, BinarySHA256 string
	Ready                                               func() bool
	Now                                                 func() time.Time
}

func (m *GroupManager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *GroupManager) Begin(ctx context.Context, id string) (GroupResult, error) {
	if !ValidRequestID(id) || m.Store == nil || m.Gateway == nil || m.HostID == "" || m.AppID == "" || m.OwnerID == "" || m.BotID == "" || m.Commit == "" || !ValidBinaryHash(m.BinarySHA256) {
		return GroupResult{}, ErrInvalid
	}
	if m.Ready == nil || !m.Ready() {
		return GroupResult{}, ErrNotReady
	}
	now := m.now()
	members, _ := json.Marshal([]string{m.OwnerID})
	name := "Codex Feishu · 验证 " + id[:8]
	g, err := m.Store.PrepareVerificationGroup(ctx, store.VerificationGroup{RequestID: id, HostID: m.HostID, AppID: m.AppID, OwnerID: m.OwnerID, BridgeCommit: m.Commit, BinarySHA256: m.BinarySHA256, UUID: store.ID(), Name: name, MembersJSON: string(members), CreatedAtMS: now.UnixMilli(), ExpiresAtMS: now.Add(5 * time.Minute).UnixMilli()})
	if err != nil {
		return GroupResult{}, err
	}
	if g.BridgeCommit != m.Commit || g.BinarySHA256 != m.BinarySHA256 || ctx.Err() != nil || m.now().UnixMilli() >= g.ExpiresAtMS {
		return m.result(g), ctx.Err()
	}
	if g.State == "ready" {
		// Validate the frozen recipient list before an API call even when a
		// caller resumes a pre-existing journal. Never inherit configured
		// additional production members in a self-only verification group.
		var frozen []string
		if json.Unmarshal([]byte(g.MembersJSON), &frozen) != nil || len(frozen) != 1 || frozen[0] != m.OwnerID || g.Name != name {
			return GroupResult{}, ErrInvalid
		}
		claimed, err := m.Store.ClaimVerificationGroup(ctx, g, m.now().UnixMilli())
		if err != nil {
			return GroupResult{}, err
		}
		if claimed {
			chat, sendErr := m.Gateway.CreateGroup(ctx, g.Name, g.OwnerID, frozen, g.UUID)
			state := "confirmed"
			if sendErr != nil || chat == "" {
				state = "unknown"
				chat = ""
				var rejected *feishu.Rejected
				if errors.As(sendErr, &rejected) {
					state = "rejected"
				}
			}
			finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			err = m.Store.FinishVerificationGroup(finishCtx, g, state, chat)
			if err == nil {
				g, err = m.Store.VerificationGroup(finishCtx, id, m.HostID, m.AppID, m.OwnerID)
			}
			cancel()
			if err != nil {
				return GroupResult{}, err
			}
		} else {
			g, err = m.Store.VerificationGroup(ctx, id, m.HostID, m.AppID, m.OwnerID)
			if err != nil {
				return GroupResult{}, err
			}
		}
	}
	if g.State != "confirmed" || g.MembershipVerifiedAtMS > 0 || ctx.Err() != nil {
		return m.result(g), nil
	}
	// Positive membership evidence is a separate, retryable GET-only step.
	// A read failure never reopens group creation or makes name matching safe.
	bot, err := m.Gateway.BotInChat(ctx, g.ChatID)
	if err != nil || !bot {
		return m.result(g), nil
	}
	ids, err := m.Gateway.MemberIDs(ctx, g.ChatID)
	if err != nil {
		return m.result(g), nil
	}
	owner := false
	for _, member := range ids {
		if member == m.OwnerID {
			owner = true
		} else if member != m.BotID {
			return m.result(g), nil
		}
	}
	if !owner {
		return m.result(g), nil
	}
	if _, err = m.Store.ObserveVerificationGroupMembership(ctx, g, m.now().UnixMilli()); err != nil {
		return GroupResult{}, err
	}
	return m.State(ctx, id)
}

func (m *GroupManager) State(ctx context.Context, id string) (GroupResult, error) {
	if !ValidRequestID(id) || m.Store == nil {
		return GroupResult{}, ErrInvalid
	}
	g, err := m.Store.VerificationGroup(ctx, id, m.HostID, m.AppID, m.OwnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return GroupResult{}, ErrNotFound
	}
	if err != nil {
		return GroupResult{}, err
	}
	return m.result(g), nil
}
func (m *GroupManager) result(g store.VerificationGroup) GroupResult {
	r := GroupResult{RequestID: g.RequestID, BridgeCommit: g.BridgeCommit, BinarySHA256: g.BinarySHA256, BuildChanged: g.BridgeCommit != m.Commit || g.BinarySHA256 != m.BinarySHA256, State: g.State, GroupStatus: "pending", MembershipStatus: "pending", GroupMessageStatus: "pending", ChatID: g.ChatID, ExpiresAtMS: g.ExpiresAtMS, Expired: m.now().UnixMilli() >= g.ExpiresAtMS}
	if g.State == "dispatching" {
		r.State = "unknown"
	}
	if g.State == "rejected" {
		r.GroupStatus = "fail"
	}
	if g.State == "confirmed" && g.ChatID != "" {
		r.GroupStatus = "pass"
		if g.MembershipVerifiedAtMS > 0 {
			r.MembershipStatus = "pass"
			if g.GroupMessageID != "" {
				r.GroupMessageStatus = "pass"
			}
		}
	}
	return r
}
func (m *GroupManager) Message(ctx context.Context, user, id, message, chat string) (bool, error) {
	if m.Store == nil || user != m.OwnerID || !ValidRequestID(id) {
		return false, nil
	}
	return m.Store.ObserveVerificationGroupMessage(ctx, m.HostID, m.AppID, m.OwnerID, id, message, chat, m.now().UnixMilli())
}
