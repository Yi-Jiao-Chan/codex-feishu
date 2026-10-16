package codexclient

import (
	"context"
	"errors"
	"strings"
)

// ListArchiveSince lists threads updated at or after the Unix-second cutoff.
// The timestamp ordering is checked before accepting the first older record as
// a pagination boundary. Unknown or out-of-order timestamps keep the scan
// conservative so a malformed page cannot hide a newer thread.
func (c *Client) ListArchiveSince(ctx context.Context, archived bool, since int64) ([]Thread, error) {
	if since <= 0 {
		return c.ListArchive(ctx, archived)
	}

	params := map[string]any{
		"limit":          100,
		"sortKey":        "updated_at",
		"sortDirection":  "desc",
		"useStateDbOnly": true,
		"archived":       archived,
		"modelProviders": []string{},
	}
	var threads []Thread
	seen := map[string]struct{}{}
	var previousUpdated int64
	havePrevious := false
	orderingVerified := true

	for {
		var response struct {
			Data       *[]Thread `json:"data"`
			NextCursor *string   `json:"nextCursor"`
		}
		if err := c.Call(ctx, "thread/list", params, &response); err != nil {
			return nil, err
		}
		if response.Data == nil {
			return nil, errors.New("invalid native thread page")
		}

		boundaryCrossed := false
		pageOrderingVerified := orderingVerified
		for _, thread := range *response.Data {
			updated := thread.UpdatedAt
			if updated <= 0 {
				// Retain an unknown-timestamp record and keep scanning. It is
				// unsafe to classify it as older than the cutoff.
				pageOrderingVerified = false
				threads = append(threads, thread)
				continue
			}
			if havePrevious && updated > previousUpdated {
				pageOrderingVerified = false
			}
			previousUpdated, havePrevious = updated, true
			if updated >= since {
				threads = append(threads, thread)
			} else {
				boundaryCrossed = true
			}
		}
		orderingVerified = pageOrderingVerified

		if response.NextCursor != nil && *response.NextCursor != "" {
			if _, exists := seen[*response.NextCursor]; exists {
				return nil, errors.New("repeated pagination cursor")
			}
		}
		if orderingVerified && boundaryCrossed {
			return threads, nil
		}
		if response.NextCursor == nil || *response.NextCursor == "" {
			return threads, nil
		}
		seen[*response.NextCursor] = struct{}{}
		params["cursor"] = *response.NextCursor
	}
}

// Sections lists all native thread sections and rejects entries without an
// identity, which cannot be used safely as a section filter.
func (c *Client) Sections(ctx context.Context) ([]ThreadSection, error) {
	sections, err := pageWithRequiredData[ThreadSection](ctx, c, "threadSection/list", map[string]any{"limit": 100})
	if err != nil {
		return nil, err
	}
	for _, section := range sections {
		if strings.TrimSpace(section.ID) == "" {
			return nil, errors.New("invalid native thread section identity")
		}
	}
	return sections, nil
}

// ListSection lists every thread in one section, in its native section order.
func (c *Client) ListSection(ctx context.Context, sectionID string, archived bool) ([]Thread, error) {
	if strings.TrimSpace(sectionID) == "" {
		return nil, errors.New("section ID is required")
	}
	return pageWithRequiredData[Thread](ctx, c, "thread/list", map[string]any{
		"sectionId":      sectionID,
		"limit":          100,
		"sortKey":        "section_position",
		"sortDirection":  "asc",
		"useStateDbOnly": true,
		"archived":       archived,
		"modelProviders": []string{},
		"sourceKinds":    allThreadSourceKinds(),
	})
}

// pageWithRequiredData is the strict variant used by scan adapters. A
// missing or null data field is a malformed page, not an empty successful
// result; this keeps a failed section scan from removing known bindings.
func pageWithRequiredData[T any](ctx context.Context, c *Client, method string, p map[string]any) ([]T, error) {
	var all []T
	seen := map[string]bool{}
	for {
		var response struct {
			Data       *[]T    `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if err := c.Call(ctx, method, p, &response); err != nil {
			return nil, err
		}
		if response.Data == nil {
			return nil, errors.New("invalid native page data")
		}
		all = append(all, (*response.Data)...)
		if response.NextCursor == nil || *response.NextCursor == "" {
			return all, nil
		}
		if seen[*response.NextCursor] {
			return nil, errors.New("repeated pagination cursor")
		}
		seen[*response.NextCursor] = true
		p["cursor"] = *response.NextCursor
	}
}

// allThreadSourceKinds matches the complete ThreadSourceKind enum in the
// verified 0.162.1 app-server schema. An explicit list is needed for section
// scans because an omitted or empty sourceKinds filter defaults to interactive
// cli/vscode sources and would hide valid appServer threads.
func allThreadSourceKinds() []string {
	return []string{
		"cli",
		"vscode",
		"exec",
		"appServer",
		"subAgent",
		"subAgentReview",
		"subAgentCompact",
		"subAgentThreadSpawn",
		"subAgentOther",
		"unknown",
	}
}
