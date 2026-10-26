package feishu

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"

	core "github.com/larksuite/oapi-sdk-go/v3/core"
)

const oneToOneGroupCountsUnknown = "one-to-one group response counts are invalid"

// IsOneToOneGroup confirms that chatID contains exactly userID and the current
// tenant bot. It uses fresh API reads; no cached membership or remote mutation.
func (g *Gateway) IsOneToOneGroup(ctx context.Context, chatID, userID string) (bool, error) {
	if strings.TrimSpace(chatID) == "" || strings.TrimSpace(userID) == "" {
		return false, errors.New("group and user identities are required")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	var r struct {
		Data *struct {
			UserCount *string `json:"user_count"`
			BotCount  *string `json:"bot_count"`
		} `json:"data"`
	}
	if err := g.request(ctx, "GET", "/open-apis/im/v1/chats/"+url.PathEscape(chatID), core.QueryParams{"user_id_type": {"open_id"}}, nil, &r); err != nil {
		return false, err
	}
	if r.Data == nil {
		return false, &Unknown{Operation: oneToOneGroupCountsUnknown}
	}
	if r.Data.UserCount == nil || r.Data.BotCount == nil {
		return false, &Unknown{Operation: oneToOneGroupCountsUnknown}
	}
	userCount, err := strconv.ParseUint(*r.Data.UserCount, 10, 64)
	if err != nil {
		return false, &Unknown{Operation: oneToOneGroupCountsUnknown}
	}
	botCount, err := strconv.ParseUint(*r.Data.BotCount, 10, 64)
	if err != nil {
		return false, &Unknown{Operation: oneToOneGroupCountsUnknown}
	}
	if userCount != 1 || botCount != 1 {
		return false, nil
	}

	present, err := g.BotInChat(ctx, chatID)
	if err != nil {
		return false, err
	}
	if !present {
		return false, nil
	}

	members, err := g.MemberIDs(ctx, chatID)
	if err != nil {
		return false, err
	}
	if len(members) != 1 || members[0] != userID {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, nil
}
