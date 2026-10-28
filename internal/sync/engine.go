package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/cot"
	"codex-feishu/internal/latency"
	"codex-feishu/internal/store"
)

type NativeHistory interface {
	Turns(context.Context, string) ([]codexclient.Turn, error)
}

// Engine converts complete public history into durable deliveries. A per-thread
// actor serializes Reconcile calls; events only trigger a new full reconciliation
// and never publish token deltas directly. Optional image preparation journals
// resource uploads before the pure renderer stages message delivery.
type Engine struct {
	NativeCOT              bool
	Store                  *store.Store
	Native                 NativeHistory
	HostID                 string
	Latency                latency.Observer
	SuppressAsyncQuestions bool
	AsyncQuestionsSinceMS  int64
	asyncItems             map[string]bool
	replySources           map[string]string
	TimeZone               string
	MaxCardBytes           int
	MentionTargets         []string // Reserve the complete structured reminder before splitting final text.
	// COTProjectionCache is a Runtime-scoped, bounded cache of already-redacted
	// tool projections. It never replaces durable mirrors, clocks or watermarks.
	COTProjectionCache *COTProjectionCache
	PrepareHuman       func(context.Context, string, []codexclient.AttachmentReference) ([]cards.AttachmentView, error)
	// PreparePublic registers local file references before they enter durable
	// cards. It changes only a display copy, never native history or input.
	PreparePublic func(context.Context, string, string) (string, error)
	// ReadUnanchoredTurns proves goal starts or manual capacity retries from native
	// session metadata. Missing input or a previous failure alone is not evidence.
	ReadUnanchoredTurns func(context.Context, string, []string) (map[string]bool, error)
	unanchoredTurns     map[string]bool
	frozenCOT           map[string]bool
	humanAttachments    map[string][]cards.AttachmentView
	// ProgressRender is pure; the actor prepares its durable control state first.
	// An empty interrupted render suppresses a new historical progress card.
	ProgressRender func(thread, turn, nativeStatus string, progress cards.ProgressView) string
	FinalRender    func(thread, turn, nativeStatus, card string) string
}

// HistoryError keeps native payloads out of ordinary error text.
type HistoryError struct{ Cause error }

func (*HistoryError) Error() string   { return "message sync awaits complete native history" }
func (e *HistoryError) Unwrap() error { return e.Cause }

func (e *Engine) Reconcile(ctx context.Context, thread string, replay bool, observedAtMS int64) error {
	if e.Native == nil {
		return errors.New("sync requires native history")
	}
	turns, err := e.Native.Turns(ctx, thread)
	if err != nil {
		return &HistoryError{Cause: err}
	}
	return e.Apply(ctx, thread, turns, replay, observedAtMS)
}

// Apply accepts only a complete, hydrated snapshot in native history order.
// Receipt links, all public documents and the scan checkpoint commit together.
func (e *Engine) Apply(ctx context.Context, thread string, turns []codexclient.Turn, replay bool, observedAtMS int64) error {
	if e.Store == nil || e.HostID == "" || thread == "" {
		return errors.New("sync requires a store and thread identity")
	}
	if e.COTProjectionCache == nil {
		e.COTProjectionCache = NewCOTProjectionCache()
	}
	// These maps describe only this complete snapshot. Apply used a value
	// receiver before the projection cache became persistent; explicitly reset
	// them so retaining the cache cannot retain stale proof or attachment state.
	e.asyncItems = nil
	e.replySources = nil
	e.unanchoredTurns = nil
	e.frozenCOT = nil
	e.humanAttachments = nil
	receipts, err := e.Store.ReceiptsForDeployment(ctx, e.HostID, thread)
	if err != nil {
		return err
	}
	old, err := e.Store.Mirrors(ctx, e.HostID, thread)
	if err != nil {
		return err
	}
	e.frozenCOT, err = e.Store.FrozenCOTMirrors(ctx, e.HostID, thread)
	if err != nil {
		return err
	}
	if ids := unanchoredProofRequired(turns, old); len(ids) > 0 && e.ReadUnanchoredTurns != nil {
		e.unanchoredTurns, err = e.ReadUnanchoredTurns(ctx, thread, ids)
		if err != nil {
			return &HistoryError{Cause: err}
		}
	}
	replySources, err := e.Store.ReplySettingsSources(ctx, e.HostID, thread)
	if err != nil {
		return err
	}
	e.replySources = replySources
	if e.SuppressAsyncQuestions {
		e.asyncItems = map[string]bool{}
		rows, readErr := e.Store.DB.QueryContext(ctx, `SELECT turn_id,item_id FROM interaction_requests WHERE host_id=? AND thread_id=? AND mode='async'`, e.HostID, thread)
		if readErr != nil {
			return readErr
		}
		for rows.Next() {
			var turn, item string
			if err := rows.Scan(&turn, &item); err != nil {
				rows.Close()
				return err
			}
			e.asyncItems[turn+":"+item] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	plan, err := e.renderSnapshot(thread, turns, receipts, old, replay, observedAtMS)
	if err != nil {
		return err
	}
	if changed, err := e.observeReplySources(ctx, thread, turns, old, plan.Docs, replay); err != nil {
		return err
	} else if changed {
		plan, err = e.renderSnapshot(thread, turns, receipts, old, replay, observedAtMS)
		if err != nil {
			return err
		}
	}
	// Validate the complete snapshot before any resource upload. The renderer
	// remains pure for recovery and reviewed legacy adoption.
	if e.PrepareHuman != nil {
		e.humanAttachments = make(map[string][]cards.AttachmentView)
		for _, turn := range turns {
			for _, item := range turn.Items {
				if item.Type != "userMessage" {
					continue
				}
				external := false
				if item.ClientID != nil {
					for _, receipt := range receipts {
						if receipt.ClientID == *item.ClientID {
							external = true
							break
						}
					}
				}
				if external {
					continue
				}
				refs := item.UserPresentation().Attachments
				if len(refs) == 0 {
					continue
				}
				views, err := e.PrepareHuman(ctx, thread, refs)
				if err != nil {
					return err
				}
				e.humanAttachments[mirrorKey("attachments", turn.ID, item.ID)] = views
			}
		}
	}
	if e.PreparePublic != nil {
		display := append([]codexclient.Turn(nil), turns...)
		for n, turn := range turns {
			display[n].Items = append([]codexclient.Item(nil), turn.Items...)
			for j, item := range turn.Items {
				if item.Type != "agentMessage" || item.Text == "" {
					continue
				}
				text, prepareErr := e.PreparePublic(ctx, thread, item.Text)
				if prepareErr != nil {
					return errors.New("public file preview preparation unavailable")
				}
				display[n].Items[j].Text = text
			}
		}
		turns = display
	}
	if e.PrepareHuman != nil || e.PreparePublic != nil {
		plan, err = e.renderSnapshot(thread, turns, receipts, old, replay, observedAtMS)
		if err != nil {
			return err
		}
	}
	started := time.Now()
	err = e.Store.StageSnapshot(ctx, e.HostID, thread, plan.Docs, plan.Links, plan.Mark)
	e.Latency.Record("mirror.commit", e.HostID, thread, "", "", time.Since(started), latency.Result(err))
	if err == nil && e.Latency != nil {
		previous := make(map[string]store.Mirror, len(old))
		for _, doc := range old {
			previous[doc.Key] = doc
		}
		for _, doc := range plan.Docs {
			before, exists := previous[doc.Key]
			hash := sha256.Sum256([]byte(doc.Card))
			if exists && before.CardHash == hex.EncodeToString(hash[:]) && before.Terminal == doc.Terminal && before.Retired == doc.Retired {
				continue
			}
			stage := ""
			switch doc.Kind {
			case "human":
				stage = "mirror.human.staged"
			case "progress":
				stage = "mirror.progress.staged"
			case "final":
				stage = "mirror.final.staged"
			}
			if stage != "" {
				e.Latency.Record(stage, e.HostID, thread, doc.TurnID, doc.Key, 0, "ok")
			}
		}
	}
	return err
}

type snapshotPlan struct {
	Docs  []store.Mirror
	Links []store.InputLink
	Mark  store.Watermark
}

// renderSnapshot shares the live renderer with reviewed legacy adoption. It does
// no database or native work and preserves the caller's existing reply anchors.
func (e Engine) renderSnapshot(thread string, turns []codexclient.Turn, receipts []store.Receipt, old []store.Mirror, replay bool, observedAtMS int64) (snapshotPlan, error) {
	asyncOnly := map[string]bool{}
	copyTurns := make([]codexclient.Turn, len(turns))
	for i, t := range turns {
		copyTurns[i] = t
		copyTurns[i].Items = nil
		items, err := uniqueItems(t.Items)
		if err != nil {
			return snapshotPlan{}, err
		}
		// Native async questions can also carry phase=final_answer. When a
		// later answer exists, keep preceding questions out of its projection
		// regardless of settings or when this turn was first observed.
		lastAnswer := -1
		for n, item := range items {
			if item.Type == "agentMessage" && !isAsyncQuestion(item) && strings.TrimSpace(item.Text) != "" && (item.Phase == nil || *item.Phase == "final_answer") {
				lastAnswer = n
			}
		}
		for n, item := range items {
			if isAsyncQuestion(item) && (n < lastAnswer || e.SuppressAsyncQuestions && (e.asyncItems[t.ID+":"+item.ID] || t.StartedAt != nil && *t.StartedAt >= e.AsyncQuestionsSinceMS/1000)) {
				asyncOnly[t.ID] = true
				continue
			}
			if item.Type == "agentMessage" {
				// Every rendering path, including preview-disabled snapshots and
				// legacy adoption, needs a readable projection of App-only embeds.
				// Validate duplicate native items above before changing this copy.
				item.Text, _ = codexclient.RewriteVisualizations(item.Text, nil)
			}
			copyTurns[i].Items = append(copyTurns[i].Items, item)
		}
	}
	turns = copyTurns

	byClient := make(map[string]store.Receipt, len(receipts))
	for _, r := range receipts {
		byClient[r.ClientID] = r
	}
	existing := make(map[string]store.Mirror, len(old))
	byTurn := make(map[string][]store.Mirror)
	for _, m := range old {
		existing[m.Key] = m
		byTurn[m.TurnID] = append(byTurn[m.TurnID], m)
	}
	mark := store.Watermark{ObservedAtMS: observedAtMS}
	var docs []store.Mirror
	var links []store.InputLink
	seenTurns := make(map[string]bool, len(turns))
	for _, turn := range turns {
		if turn.ID == "" || seenTurns[turn.ID] || turn.ItemsView != "" && turn.ItemsView != "full" {
			return snapshotPlan{}, errors.New("sync requires unique full turns")
		}
		seenTurns[turn.ID] = true
		switch turn.Status {
		case "inProgress", "completed", "failed", "interrupted":
		default:
			return snapshotPlan{}, errors.New("unsupported native turn status")
		}
		items, err := uniqueItems(turn.Items)
		if err != nil {
			return snapshotPlan{}, err
		}
		turn.Items = items
		mark.TurnID = turn.ID
		var parentKey, parentID string
		direct := false
		firstInput := true
		active := map[string]bool{}
		for _, i := range turn.Items {
			mark.ItemID = i.ID
			if i.Type != "userMessage" {
				continue
			}
			if i.ClientID != nil {
				if r, ok := byClient[*i.ClientID]; ok {
					links = append(links, store.InputLink{ReceiptID: r.ID, TurnID: turn.ID, ItemID: i.ID})
					if firstInput {
						parentID, firstInput = r.ReplyID(), false
					}
					continue // Feishu's original message is already human context.
				}
			}
			presentation := i.UserPresentation()
			text := presentation.Text
			views, prepared := e.humanAttachments[mirrorKey("attachments", turn.ID, i.ID)]
			if !prepared {
				views = attachmentPlaceholders(presentation.Attachments)
			}
			if strings.TrimSpace(text) == "" && len(views) == 0 {
				text = "此条输入没有可同步的文本，请在 Codex 中查看。"
			}
			parts, err := cards.SplitHuman("Codex 端", text, views, markerCardBudget(e.MaxCardBytes))
			if err != nil {
				return snapshotPlan{}, err
			}
			for n, part := range parts {
				key := mirrorKey("human", turn.ID, i.ID, fmt.Sprint(n))
				docs = append(docs, store.Mirror{Key: key, TurnID: turn.ID, Kind: "human", Card: part.Card, Terminal: true, Replay: replay})
				active[key] = true
				if firstInput {
					parentKey, firstInput = key, false
				}
			}
		}
		// Once created, a turn's native reply anchor cannot move when the
		// history includes a subsequent steer input or changes projections.
		for _, m := range byTurn[turn.ID] {
			if m.Kind != "human" {
				parentKey, parentID = m.ParentKey, m.ParentMessageID
				break
			}
		}
		if parentKey == "" && parentID == "" {
			// Empty drafts and standalone compaction turns have no public
			// message or reply anchor. Their items still advance the scan
			// checkpoint. Public turns need an input anchor or verified native origin.
			internalOnly := true
			for _, item := range turn.Items {
				if item.Type != "contextCompaction" {
					internalOnly = false
				}
			}
			if internalOnly {
				continue
			}
			if !e.unanchoredTurns[turn.ID] {
				return snapshotPlan{}, errors.New("public turn has no initiating human input")
			}
			// Proven goal starts and manual capacity retries need no new human
			// message. Deliver directly without borrowing another turn's anchor.
			direct = true
		}
		key := mirrorKey("progress", turn.ID)
		previousProgress, hadProgress := existing[key]
		previousVersion, previousCOT := 0, false
		if hadProgress {
			if previousProgress.Card == "" {
				previousVersion = previousProgress.ProjectionVersion
				previousCOT = previousVersion != 0
			} else {
				previousDocument, nativeCOT, parseErr := cot.Parse(previousProgress.Card)
				if parseErr != nil {
					return snapshotPlan{}, parseErr
				}
				previousCOT = nativeCOT
				if nativeCOT {
					previousVersion = previousDocument.Version
				}
			}
		}
		// The store already ignores all changes after the terminal event batch
		// was acknowledged. Omit only that immutable progress document instead
		// of reprojecting every old tool and discarding the result. Full history
		// validation, input links, final corrections and the watermark still run.
		frozenProgress := previousCOT && previousProgress.Terminal && e.frozenCOT[key] &&
			(previousVersion == cot.LegacyVersion || previousVersion == cot.Version)
		if !frozenProgress && (turn.Status == "inProgress" || hadProgress || turn.Status == "interrupted" && e.ProgressRender != nil) {
			render := func(progress cards.ProgressView) string {
				return cards.Task(cards.TaskView{Status: turn.Status, Progress: progress})
			}
			if e.ProgressRender != nil {
				render = func(progress cards.ProgressView) string {
					return e.ProgressRender(thread, turn.ID, turn.Status, progress)
				}
			}
			clocks := make(map[string]int64, len(previousProgress.ProgressObservedAt))
			for id, at := range previousProgress.ProgressObservedAt {
				clocks[id] = at
			}
			var card string
			var err error
			if previousCOT || e.NativeCOT && !hadProgress && turn.Status == "inProgress" {
				version := cot.Version
				if previousCOT {
					version = previousVersion
				}
				// Native COT builds its own message and tool clocks. Avoid the
				// legacy ProgressView scan and its duplicate command summaries.
				card, err = cotDocumentVersionWithCache(thread, turn, clocks, observedAtMS, version, e.COTProjectionCache)
			} else {
				view := progressView(turn, observedAtMS)
				view.TimeZone = e.TimeZone
				for n := range view.Records {
					record := &view.Records[n]
					if record.Tools != nil {
						continue // Tool groups do not have public-message reception clocks.
					}
					at := clocks[record.ItemID]
					if at <= 0 {
						// This is the first complete snapshot that records a local
						// reception time, including replay and legacy unknown clocks.
						// Persist it once; it is not the native generation time.
						at = observedAtMS
						clocks[record.ItemID] = at
					}
					record.ReceivedAtMS = at
				}
				card, err = boundedRenderedProgress(view, markerCardBudget(e.MaxCardBytes), render)
			}
			if err != nil {
				return snapshotPlan{}, err
			}
			if card != "" {
				docs = append(docs, store.Mirror{Key: key, TurnID: turn.ID, Kind: "progress", Card: card, ProgressObservedAt: clocks, Terminal: turn.Status != "inProgress", Replay: replay, Direct: direct, ParentKey: parentKey, ParentMessageID: parentID})
			}
		}
		body := finalBody(turn)
		suppressInterruptedEmptyFallback := turn.Status == "interrupted" && previousCOT && previousProgress.RemoteID != "" && strings.TrimSpace(body) == ""
		if turn.Status != "inProgress" && !suppressInterruptedEmptyFallback && !(turn.Status == "completed" && asyncOnly[turn.ID] && strings.TrimSpace(body) == "") {
			if strings.TrimSpace(body) == "" {
				body = emptyResult(turn.Status)
			}
			budget, err := finalCardBudget(e.MaxCardBytes, e.MentionTargets)
			if err != nil {
				return snapshotPlan{}, err
			}
			replySource := replySourceForTurn(turn.ID, e.replySources)
			replyRole := "AI 回复"
			if stored, ok := e.replySources[turn.ID]; ok && stored != "" {
				replyRole = ""
			}
			parts, err := cards.SplitMessage(replyRole, replySource, body, budget)
			if err != nil {
				return snapshotPlan{}, err
			}
			if e.FinalRender != nil && len(parts) > 0 {
				decorated := e.FinalRender(thread, turn.ID, turn.Status, parts[0].Card)
				if len(decorated) > budget {
					// Controls count toward the same complete JSON byte budget.
					// Reserve their measured overhead before splitting again.
					reserve := len(decorated) - len(parts[0].Card)
					parts, err = cards.SplitMessage(replyRole, replySource, body, budget-reserve)
					if err != nil {
						return snapshotPlan{}, err
					}
					decorated = e.FinalRender(thread, turn.ID, turn.Status, parts[0].Card)
				}
				if len(decorated) > budget || !json.Valid([]byte(decorated)) {
					return snapshotPlan{}, errors.New("final renderer returned invalid or oversized card")
				}
				parts[0].Card = decorated
			}
			for n, part := range parts {
				key := mirrorKey("final", turn.ID, fmt.Sprint(n))
				doc := store.Mirror{Key: key, TurnID: turn.ID, Kind: "final", Card: part.Card, Terminal: true, Replay: replay, Direct: direct, ParentKey: parentKey, ParentMessageID: parentID}
				if n == 0 && (turn.Status == "completed" || turn.Status == "failed") {
					doc.EventType = turn.Status
					if turn.CompletedAt != nil && *turn.CompletedAt > 0 && *turn.CompletedAt <= observedAtMS/1000 {
						doc.EventAt = *turn.CompletedAt // Native 0.162.0 uses seconds, not milliseconds.
					}
				}
				docs = append(docs, doc)
				active[key] = true
			}
		}
		// A corrected full answer may use fewer segments. Unsent tails are
		// suppressed locally; existing cards lose the obsolete text via PATCH.
		for _, m := range byTurn[turn.ID] {
			if m.Kind == "progress" || active[m.Key] || m.Kind == "final" && turn.Status == "inProgress" {
				continue
			}
			m.Retired, m.Terminal = true, true
			m.Card = cards.Message(role(m.Kind), source(m.Kind), "此分片内容已更新并合并到前文。")
			docs = append(docs, m)
		}
	}
	return snapshotPlan{Docs: docs, Links: links, Mark: mark}, nil
}

func unanchoredProofRequired(turns []codexclient.Turn, old []store.Mirror) []string {
	previouslyUnanchored := map[string]bool{}
	hasPrevious := map[string]bool{}
	for _, m := range old {
		if m.Kind != "human" && !hasPrevious[m.TurnID] {
			hasPrevious[m.TurnID] = true
			previouslyUnanchored[m.TurnID] = m.ParentKey == "" && m.ParentMessageID == ""
		}
	}
	var ids []string
	for _, turn := range turns {
		human, public := false, false
		for _, item := range turn.Items {
			human = human || item.Type == "userMessage"
			public = public || item.Type != "contextCompaction"
		}
		if public && (previouslyUnanchored[turn.ID] || !hasPrevious[turn.ID] && !human) {
			ids = append(ids, turn.ID)
		}
	}
	return ids
}

func mirrorKey(kind string, parts ...string) string {
	encoded, _ := json.Marshal(parts)
	sum := sha256.Sum256(encoded)
	return kind + ":" + hex.EncodeToString(sum[:])
}

// Reject conflicting duplicates from an inconsistent paginated snapshot; exact
// duplicates are harmless. Legacy IDs may repeat across turns, not within one.
func uniqueItems(items []codexclient.Item) ([]codexclient.Item, error) {
	seen := make(map[string]codexclient.Item, len(items))
	result := make([]codexclient.Item, 0, len(items))
	for _, i := range items {
		if i.ID == "" {
			return nil, errors.New("native history item identity absent")
		}
		if old, ok := seen[i.ID]; ok {
			if !reflect.DeepEqual(old, i) {
				return nil, errors.New("conflicting duplicate native history item")
			}
			continue
		}
		seen[i.ID] = i
		result = append(result, i)
	}
	return result, nil
}

func isAsyncQuestion(item codexclient.Item) bool {
	return item.Type == "agentMessage" && item.Delivery != nil && *item.Delivery == "async" && len(item.Questions) > 0
}

func finalBody(t codexclient.Turn) string {
	var texts []string
	legacy := ""
	for _, i := range t.Items {
		if i.Type != "agentMessage" || strings.TrimSpace(i.Text) == "" {
			continue
		}
		if i.Phase != nil && *i.Phase == "final_answer" {
			texts = append(texts, i.Text)
		} else if i.Phase == nil {
			legacy = i.Text
		}
	}
	if len(texts) == 0 {
		return legacy
	}
	return strings.Join(texts, "\n\n")
}

// progressView projects public commentary and adjacent tool groups in native
// item order, without retaining tool arguments, paths or output.
// The complete snapshot replaces the previous projection rather than appending
// deltas. A legacy terminal's selected final item is not also a process record.
func progressView(t codexclient.Turn, observedAtMS int64) cards.ProgressView {
	v := cards.ProgressView{ElapsedSeconds: -1}
	legacyFinal, explicitFinal := -1, false
	if t.Status != "inProgress" {
		for n, i := range t.Items {
			if i.Type != "agentMessage" || strings.TrimSpace(i.Text) == "" {
				continue
			}
			if i.Phase == nil {
				legacyFinal = n
			} else if *i.Phase == "final_answer" {
				explicitFinal = true
			}
		}
	}
	var tools *cards.ProgressRecord
	flushTools := func() {
		if tools != nil {
			v.Records = append(v.Records, *tools)
			tools = nil
		}
	}
	for n, i := range t.Items {
		if labels, ok := progressToolLabels(i); ok {
			if tools == nil {
				tools = &cards.ProgressRecord{ItemID: i.ID, Tools: &cards.ProgressTools{}}
			}
			before := *tools.Tools
			addProgressTool(tools.Tools, i, labels)
			// Preserve the legacy counters: FinishedTools includes every known
			// terminal state, including failed and declined, not only successes.
			v.RunningTools += tools.Tools.Running - before.Running
			v.FinishedTools += tools.Tools.Completed + tools.Tools.Failed + tools.Tools.Declined - before.Completed - before.Failed - before.Declined
			continue
		}
		if i.Type == "agentMessage" || i.Type == "userMessage" {
			flushTools() // Final replies and private user input are boundaries too.
		}
		if i.Type != "agentMessage" || strings.TrimSpace(i.Text) == "" ||
			(i.Phase != nil && *i.Phase != "commentary" && *i.Phase != "partial_answer") ||
			(n == legacyFinal && !explicitFinal) {
			continue
		}
		v.Records = append(v.Records, cards.ProgressRecord{ItemID: i.ID, Text: i.Text})
		v.Body = i.Text
		if t.Status == "inProgress" {
			v.CurrentItemID = i.ID
		}
	}
	flushTools()
	runes := []rune(v.Body)
	if len(runes) > 800 {
		v.Body = string(runes[:800]) + "…（内容已截短）"
	}
	if t.Status != "inProgress" {
		v.Body = "" // Keep completed process details in the collapsed records.
		v.CurrentItemID = ""
	}
	// Native turn timestamps are seconds. Terminal duration never advances on
	// replay, and missing/invalid timestamps never become an invented duration.
	observed := observedAtMS / 1000
	if t.StartedAt != nil && *t.StartedAt > 0 && observed >= *t.StartedAt {
		end := observed
		if t.Status != "inProgress" {
			if t.CompletedAt == nil {
				return v
			}
			end = *t.CompletedAt
		}
		if end >= *t.StartedAt && end <= observed {
			v.ElapsedSeconds = end - *t.StartedAt
		}
	}
	return v
}

func boundedRenderedProgress(v cards.ProgressView, limit int, render func(cards.ProgressView) string) (string, error) {
	if limit == 0 {
		limit = cards.DefaultMaxCardBytes
	}
	// Copy mutable display state before truncation. Tool metadata may be shared
	// with the complete projection used again by the next render/reconciliation.
	v.Records = append([]cards.ProgressRecord(nil), v.Records...)
	for n := range v.Records {
		if v.Records[n].Tools != nil {
			copyTools := *v.Records[n].Tools
			copyTools.Details = append([]string(nil), copyTools.Details...)
			copyTools.Items = append([]cards.ProgressTool(nil), copyTools.Items...)
			for index := range copyTools.Items {
				copyTools.Items[index].Details = append([]string(nil), copyTools.Items[index].Details...)
			}
			v.Records[n].Tools = &copyTools
		}
	}
	if omitted := len(v.Records) - cards.MaxProgressRecords; omitted > 0 {
		v.Records = v.Records[omitted:]
		v.OmittedRecords += omitted
	}
	card := render(v)
	if card == "" {
		return "", nil
	}
	if !json.Valid([]byte(card)) {
		return "", errors.New("progress renderer returned invalid card")
	}
	if len(card) <= limit {
		return card, nil
	}
	// Prefer the newest complete records. The omission count remains explicit
	// even if none of the process records fit this card's budget.
	if len(v.Records) > 1 {
		// The pure renderer's fixed state and controls do not change while
		// evicting records. Search the oldest prefix instead of serializing
		// every remaining suffix of a long native history.
		records, omitted := v.Records, v.OmittedRecords
		lo, hi := 1, len(records)-1
		for lo < hi {
			mid := (lo + hi) / 2
			v.Records, v.OmittedRecords = records[mid:], omitted+mid
			if len(render(v)) <= limit {
				hi = mid
			} else {
				lo = mid + 1
			}
		}
		v.Records, v.OmittedRecords = records[lo:], omitted+lo
		card = render(v)
	}
	shorten := func(text string, set func(string)) bool {
		runes := []rune(text)
		set("…（内容已截短）")
		if len(render(v)) > limit {
			return false
		}
		lo, hi := 0, len(runes)
		for lo < hi {
			mid := (lo + hi + 1) / 2
			set(string(runes[:mid]) + "…（内容已截短）")
			if len(render(v)) <= limit {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		set(string(runes[:lo]) + "…（内容已截短）")
		return true
	}
	if len(card) > limit && len(v.Records) == 1 {
		if v.Records[0].Tools != nil {
			// Keep full counters while evicting the oldest retained calls. The
			// renderer may already hide some for its component budget, so use a
			// bounded loop rather than assuming monotonic JSON byte sizes.
			tools := v.Records[0].Tools
			for len(card) > limit && len(tools.Items) > 0 {
				tools.Items = tools.Items[1:]
				tools.OmittedItems++
				card = render(v)
			}
			if len(card) > limit {
				v.Records = nil
				v.OmittedRecords++
			}
		} else if !shorten(v.Records[0].Text, func(s string) { v.Records[0].Text = s }) {
			v.Records = nil
			v.OmittedRecords++
		}
		card = render(v)
	}
	if len(card) > limit {
		body := v.Body
		v.Body = ""
		if len(render(v)) > limit {
			return "", errors.New("card limit cannot hold progress status and omission notice")
		}
		if !shorten(body, func(s string) { v.Body = s }) {
			v.Body = ""
		}
		card = render(v)
	}
	if !json.Valid([]byte(card)) || len(card) > limit {
		return "", errors.New("progress renderer returned invalid or oversized card")
	}
	return card, nil
}

func emptyResult(status string) string {
	switch status {
	case "failed":
		return "本轮执行失败，请在 Codex App 中查看详情。"
	case "interrupted":
		return "本轮已停止。"
	default:
		return "本轮已结束，未返回公开回复。"
	}
}

func role(kind string) string {
	if kind == "human" {
		return "人类发送"
	}
	return "AI 回复"
}
func source(kind string) string {
	if kind == "human" {
		return "Codex 端"
	}
	return "Codex"
}

// FinalDelivered only proves all current result cards were confirmed. Native
// completion alone must not turn the originating input's reaction into a check.
func (e Engine) FinalDelivered(ctx context.Context, thread, turn string) (bool, error) {
	mirrors, err := e.Store.Mirrors(ctx, e.HostID, thread)
	if err != nil {
		return false, err
	}
	found := false
	for _, m := range mirrors {
		if m.TurnID != turn || m.Kind != "final" {
			continue
		}
		if m.Retired && m.RemoteID == "" && m.State == "confirmed" {
			continue
		}
		if !m.Retired {
			found = true
		}
		if m.State != "confirmed" || m.RemoteID == "" || m.DeliveredRevision != m.Revision {
			return false, nil
		}
	}
	return found, nil
}

// The persisted wire card later adds one invisible request-marker field.
func markerCardBudget(limit int) int {
	if limit == 0 {
		limit = cards.DefaultMaxCardBytes
	}
	if limit <= cards.MaxRequestMarkerBytes {
		return -1
	}
	return limit - cards.MaxRequestMarkerBytes
}

func finalCardBudget(limit int, targets []string) (int, error) {
	budget := markerCardBudget(limit)
	if len(targets) == 0 {
		return budget, nil
	}
	base := cards.Message("", "", "")
	mentioned, err := cards.WithMentions(base, targets)
	if err != nil {
		return 0, errors.New("notification targets exceed card budget")
	}
	budget -= len(mentioned) - len(base) + cards.MaxMentionBlockExtraBytes
	if budget <= 0 {
		return 0, errors.New("card budget cannot hold notification targets")
	}
	return budget, nil
}
