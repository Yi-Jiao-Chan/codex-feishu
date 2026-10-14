package codexclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

type Thread struct {
	ID             string          `json:"id"`
	Path           *string         `json:"path"`
	CWD            string          `json:"cwd"`
	Name           *string         `json:"name"`
	Preview        string          `json:"preview"`
	CreatedAt      int64           `json:"createdAt"`
	UpdatedAt      int64           `json:"updatedAt"`
	ParentThreadID *string         `json:"parentThreadId"`
	ForkedFromID   *string         `json:"forkedFromId"`
	ProjectID      *string         `json:"projectId"`
	Ephemeral      bool            `json:"ephemeral"`
	ThreadSource   json.RawMessage `json:"threadSource"`
	Source         json.RawMessage `json:"source"`
	Originator     *string         `json:"originator"`
	Status         struct {
		Type string `json:"type"`
	} `json:"status"`
	Turns []Turn `json:"turns"`
}
type Turn struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Items       []Item `json:"items"`
	ItemsView   string `json:"itemsView"`
	StartedAt   *int64 `json:"startedAt"`
	CompletedAt *int64 `json:"completedAt"`
}
type AsyncQuestion struct {
	Title   string   `json:"title"`
	Options []string `json:"options"`
}
type FileChange struct {
	Path string          `json:"path"`
	Kind json.RawMessage `json:"kind"`
	Diff string          `json:"diff"`
}
type Item struct {
	Delivery  *string           `json:"delivery"`
	Questions []AsyncQuestion   `json:"questions"`
	Command   string            `json:"command"`
	CWD       string            `json:"cwd"`
	Changes   []FileChange      `json:"changes"`
	Type      string            `json:"type"`
	ID        string            `json:"id"`
	Text      string            `json:"text"`
	Phase     *string           `json:"phase"`
	Content   []json.RawMessage `json:"content"`
	ClientID  *string           `json:"clientId"`
	Status    string            `json:"status"`
}

func (i Item) PublicText() string {
	if i.Type == "agentMessage" {
		return i.Text
	}
	if i.Type != "userMessage" {
		return ""
	}
	var parts []string
	for _, b := range i.Content {
		var p struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(b, &p) == nil && p.Type == "text" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n")
}

type Queued struct {
	ID                  string          `json:"id"`
	Input               json.RawMessage `json:"input"`
	ClientUserMessageID *string         `json:"clientUserMessageId"`
}

func TextInput(s string) []map[string]any {
	return []map[string]any{{"type": "text", "text": s, "text_elements": []any{}}}
}
func (c *Client) Attach(ctx context.Context, id string) (Thread, error) {
	var r struct {
		Thread Thread `json:"thread"`
	}
	err := c.Call(ctx, "thread/resume", map[string]any{"threadId": id, "excludeTurns": true}, &r)
	return r.Thread, err
}
func (c *Client) Metadata(ctx context.Context, id string) (Thread, error) {
	var r struct {
		Thread Thread `json:"thread"`
	}
	err := c.Call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false}, &r)
	return r.Thread, err
}

// page walks opaque cursors, rejecting loops rather than silently truncating.
func page[T any](ctx context.Context, c *Client, method string, p map[string]any) ([]T, error) {
	var all []T
	seen := map[string]bool{}
	for {
		var r struct {
			Data       []T     `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if err := c.Call(ctx, method, p, &r); err != nil {
			return nil, err
		}
		all = append(all, r.Data...)
		if r.NextCursor == nil || *r.NextCursor == "" {
			return all, nil
		}
		if seen[*r.NextCursor] {
			return nil, errors.New("repeated pagination cursor")
		}
		seen[*r.NextCursor] = true
		p["cursor"] = *r.NextCursor
	}
}
func (c *Client) List(ctx context.Context) ([]Thread, error) {
	return c.ListArchive(ctx, false)
}

// An empty provider filter includes all configured providers. Omitting it lets
// the server filter to the current provider and is unsuitable for a baseline.
func (c *Client) ListArchive(ctx context.Context, archived bool) ([]Thread, error) {
	return page[Thread](ctx, c, "thread/list", map[string]any{"limit": 100, "sortKey": "updated_at", "sortDirection": "desc", "useStateDbOnly": true, "archived": archived, "modelProviders": []string{}})
}

// LoadedIDs includes unmaterialized drafts which thread/list may omit. Reading
// metadata does not subscribe to events; drafts must remain pending until input.
func (c *Client) LoadedIDs(ctx context.Context) ([]string, error) {
	return page[string](ctx, c, "thread/loaded/list", map[string]any{"limit": 100})
}

func BeforeFirstInput(err error) bool {
	var rpc *RPCError
	return errors.As(err, &rpc) && rpc.Code == -32600 && strings.Contains(rpc.Message, "thread/turns/list is unavailable before first user message")
}
func (c *Client) Turns(ctx context.Context, id string) ([]Turn, error) {
	turns, err := page[Turn](ctx, c, "thread/turns/list", map[string]any{"threadId": id, "limit": 50, "sortDirection": "asc", "itemsView": "full"})
	if err != nil {
		return nil, err
	}
	for index := range turns {
		turn := &turns[index]
		switch turn.ItemsView {
		case "", "full": // Missing view is retained for legacy fixture compatibility.
			continue
		case "summary", "notLoaded":
		default:
			return nil, errors.New("unsupported turn items view")
		}
		entries, err := page[struct {
			TurnID string `json:"turnId"`
			Item   Item   `json:"item"`
		}](ctx, c, "thread/items/list", map[string]any{"threadId": id, "turnId": turn.ID, "limit": 100, "sortDirection": "asc"})
		if err != nil {
			return nil, err
		}
		items := make([]Item, 0, len(entries))
		for _, entry := range entries {
			if entry.TurnID != turn.ID {
				return nil, errors.New("item history returned another turn")
			}
			items = append(items, entry.Item)
		}
		turn.Items = items
		turn.ItemsView = "full"
	}
	return turns, nil
}
func (c *Client) Queue(ctx context.Context, id string) ([]Queued, error) {
	return page[Queued](ctx, c, "thread/queue/list", map[string]any{"threadId": id, "limit": 100})
}
func (c *Client) Add(ctx context.Context, id, text, clientID string) (Queued, error) {
	var r struct {
		Queued Queued `json:"queuedSubmission"`
	}
	err := c.Call(ctx, "thread/queue/add", map[string]any{"threadId": id, "input": TextInput(text), "clientUserMessageId": clientID}, &r)
	return r.Queued, err
}
func (c *Client) Delete(ctx context.Context, id, qid string) (bool, error) {
	var r struct {
		Deleted bool `json:"deleted"`
	}
	err := c.Call(ctx, "thread/queue/delete", map[string]any{"threadId": id, "queuedSubmissionId": qid}, &r)
	return r.Deleted, err
}
func (c *Client) Steer(ctx context.Context, id, turn, text, clientID string) error {
	return c.Call(ctx, "turn/steer", map[string]any{"threadId": id, "expectedTurnId": turn, "input": TextInput(text), "clientUserMessageId": clientID}, nil)
}
func (c *Client) Interrupt(ctx context.Context, id, turn string) error {
	return c.Call(ctx, "turn/interrupt", map[string]any{"threadId": id, "turnId": turn}, nil)
}
func (c *Client) StartQueue(ctx context.Context, id string) error {
	return c.Call(ctx, "thread/queue/start", map[string]any{"threadId": id}, nil)
}
