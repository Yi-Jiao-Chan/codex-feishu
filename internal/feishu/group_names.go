package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"unicode/utf8"

	core "github.com/larksuite/oapi-sdk-go/v3/core"
)

type GroupInfo struct {
	Name, OwnerID, ChatMode, Description string
}

func (g *Gateway) GetGroupInfo(ctx context.Context, id string) (GroupInfo, error) {
	if strings.TrimSpace(id) == "" {
		return GroupInfo{}, errors.New("group identity is required")
	}
	var r struct {
		Data *struct {
			Name        *string `json:"name"`
			OwnerID     *string `json:"owner_id"`
			ChatMode    *string `json:"chat_mode"`
			Description *string `json:"description"`
		} `json:"data"`
	}
	err := g.request(ctx, "GET", "/open-apis/im/v1/chats/"+url.PathEscape(id), core.QueryParams{"user_id_type": {"open_id"}}, nil, &r)
	if err != nil {
		return GroupInfo{}, err
	}
	if r.Data == nil || r.Data.Name == nil || r.Data.OwnerID == nil || *r.Data.OwnerID == "" || r.Data.ChatMode == nil || *r.Data.ChatMode == "" || r.Data.Description == nil {
		return GroupInfo{}, &Unknown{"group information response is incomplete"}
	}
	return GroupInfo{Name: *r.Data.Name, OwnerID: *r.Data.OwnerID, ChatMode: *r.Data.ChatMode, Description: *r.Data.Description}, nil
}

// UpdateGroupName changes only name. In particular, creation's immutable
// description, UUID, owner and member scope are not copied into this request.
func (g *Gateway) UpdateGroupName(ctx context.Context, id, name string) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(name) == "" || !utf8.ValidString(name) {
		return errors.New("group identity and a valid nonempty name are required")
	}
	var r struct {
		Data *json.RawMessage `json:"data"`
	}
	err := g.request(ctx, "PUT", "/open-apis/im/v1/chats/"+url.PathEscape(id), core.QueryParams{"user_id_type": {"open_id"}}, map[string]string{"name": name}, &r)
	if err == nil && (r.Data == nil || !strings.HasPrefix(strings.TrimSpace(string(*r.Data)), "{")) {
		return &Unknown{"group name update response is incomplete"}
	}
	return err
}
