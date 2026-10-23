package store

import (
	"context"
	"strings"
	"testing"
)

func TestInputContinueTokenRequiresExactServerReceiptAndSource(t *testing.T) {
	receipt, source := ID(), ID()
	valid := SubmissionAction{Kind: "continue", ReceiptID: receipt, Token: inputContinuePrefix + receipt + "." + source}
	if got, ok := InputContinueSource(valid); !ok || got != source {
		t.Fatal("server-generated stop reference was not recognized")
	}
	app := valid
	app.Token, app.ExpectedTurnID = inputContinuePrefix+receipt+".app", "observed-turn"
	if got, ok := InputContinueSource(app); !ok || got != "" {
		t.Fatal("frozen App interruption was not recognized")
	}
	for _, modify := range []func(*SubmissionAction){
		func(a *SubmissionAction) { a.Kind = "stop" },
		func(a *SubmissionAction) { a.ReceiptID = ID() },
		func(a *SubmissionAction) { a.Token += ".extra" },
		func(a *SubmissionAction) { a.Token = "other." + receipt + "." + source },
		func(a *SubmissionAction) { a.Token = inputContinuePrefix + receipt + "." + strings.ToUpper(source) },
		func(a *SubmissionAction) { a.Token = inputContinuePrefix + receipt + ".unknown" },
		func(a *SubmissionAction) { a.Token = inputContinuePrefix + receipt + ".app" },
	} {
		bad := valid
		modify(&bad)
		if _, ok := InputContinueSource(bad); ok {
			t.Fatal("malformed token or non-input control was recognized")
		}
	}
}

func TestOrdinaryContinueAdmissionRollsBackWithItsReceipt(t *testing.T) {
	s, _, scope := taskControlStore(t, true)
	ctx := context.Background()
	if _, err := s.ClaimThreadSubmissionActionExpected(ctx, scope, "stop", "synthetic-stop", "turn"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_input_continue BEFORE INSERT ON submission_actions WHEN NEW.token LIKE 'input-continue.%' BEGIN SELECT RAISE(ABORT,'synthetic save failure'); END`); err != nil {
		t.Fatal(err)
	}
	in := Receipt{AppID: scope.AppID, ThreadID: scope.ThreadID, ChatID: "chat", MessageID: "synthetic-input", Body: "synthetic content"}
	if _, _, err := s.ReceiveSubmissionInput(ctx, scope, in); err == nil {
		t.Fatal("failed continue intent silently accepted ordinary input")
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM m0_receipts WHERE message_id='synthetic-input'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("receipt escaped failed admission transaction", err)
	}
	if _, err := s.DB.Exec(`DROP TRIGGER fail_input_continue`); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := s.ReceiveSubmissionInput(ctx, scope, in); err != nil || !fresh {
		t.Fatal("definitely failed local admission could not be received", err)
	}
}

func TestAttachmentOrdinaryInputCapturesContinueAtAdmissionOnly(t *testing.T) {
	for _, mode := range []string{"text", "new-attachment", "old-collecting", "old-draft-new-text"} {
		t.Run(mode, func(t *testing.T) {
			s, _, scope := taskControlStore(t, true)
			ctx := context.Background()
			in := AttachmentInput{Scope: scope, ChatID: "chat", SenderID: "synthetic-operator", MessageID: "synthetic-message", Body: "synthetic content", Parts: []AttachmentPart{{Kind: "text", Text: "synthetic content"}}}
			if mode != "text" {
				in.Parts = append(in.Parts, AttachmentPart{Kind: "file", ResourceKey: "synthetic-file", Name: "synthetic.txt"})
			}
			var batch AttachmentBatch
			if mode == "old-collecting" || mode == "old-draft-new-text" {
				if mode == "old-draft-new-text" {
					in.Body, in.Parts = "", in.Parts[1:]
				}
				var err error
				batch, _, err = s.ReceiveAttachmentInput(ctx, in, 10, 5)
				if err != nil {
					t.Fatal(err)
				}
			}
			stop, err := s.ClaimThreadSubmissionActionExpected(ctx, scope, "stop", "synthetic-stop", "turn")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "old-collecting" {
				tx, err := s.DB.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err = finishAttachmentReceiptTx(ctx, tx, scope, batch.ReceiptID, "received", `[{"type":"text","text":"synthetic downloaded input"}]`); err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
			} else {
				if mode == "old-draft-new-text" {
					in.MessageID, in.Body, in.ExpectedBatch = "fresh-explanation", "synthetic explanation", batch.ID
					in.Parts = []AttachmentPart{{Kind: "text", Text: in.Body}}
				}
				if _, fresh, err := s.ReceiveAttachmentInput(ctx, in, 10, 5); err != nil || !fresh {
					t.Fatal("attachment ordinary input was not accepted", err)
				}
			}
			if err := s.StageSubmissions(ctx, scope); err != nil {
				t.Fatal(err)
			}
			actions, err := s.SubmissionActions(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			continued := 0
			for _, a := range actions {
				if source, ok := InputContinueSource(a); ok {
					continued++
					if source != stop.ID {
						t.Fatal("input bound to the wrong stop")
					}
				}
			}
			if mode == "old-collecting" && continued != 0 || mode != "old-collecting" && continued != 1 {
				t.Fatal("attachment readiness replaced the fresh-message admission boundary")
			}
		})
	}
}

func TestTaskStopClaimAndNativeIntentCommitAtomically(t *testing.T) {
	s, _, scope := taskControlStore(t, true)
	ctx := context.Background()
	if err := s.StageTaskControls(ctx, scope, []TaskControl{{TurnID: "turn", NativeStatus: "inProgress", Kind: "stop", State: "available", QueueCount: -1}}); err != nil {
		t.Fatal(err)
	}
	v := taskControlView(t, s, scope)
	stageTaskCard(t, s, scope, v, true, 1)
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_task_claim BEFORE INSERT ON task_control_claims BEGIN SELECT RAISE(ABORT,'synthetic task claim failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTaskControl(ctx, scope.HostID, scope.AppID, "chat", "actual-progress", "stop", v.Token, v.Revision); err == nil {
		t.Fatal("failed task claim unexpectedly succeeded")
	}
	actions, err := s.SubmissionActions(ctx, scope)
	if err != nil || len(actions) != 0 || taskControlView(t, s, scope).State != "available" {
		t.Fatal("native stop escaped rolled-back card claim", err)
	}
	if _, err := s.DB.Exec(`DROP TRIGGER fail_task_claim`); err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimTaskControl(ctx, scope.HostID, scope.AppID, "chat", "actual-progress", "stop", v.Token, v.Revision)
	if err != nil || claim.NativeActionID == "" {
		t.Fatal("accepted task stop has no atomic native action", err)
	}
	r, _, err := s.ReceiveSubmissionInput(ctx, scope, Receipt{AppID: scope.AppID, ThreadID: scope.ThreadID, ChatID: "chat", MessageID: "following-input", Body: "synthetic content"})
	if err != nil {
		t.Fatal(err)
	}
	actions, err = s.SubmissionActions(ctx, scope)
	if err != nil || len(actions) != 2 {
		t.Fatal("following message could not observe atomic stop", err)
	}
	if source, ok := InputContinueSource(actions[1]); !ok || source != claim.NativeActionID || actions[1].ReceiptID != r.ID {
		t.Fatal("message fell between card stop claim and native intent")
	}
}

func TestAttachmentSealingPreservesEarlierUnstagedReceiptFIFO(t *testing.T) {
	for _, existingDraft := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-attachment", true: "existing-draft"}[existingDraft], func(t *testing.T) {
			s, _, scope := taskControlStore(t, true)
			ctx := context.Background()
			in := AttachmentInput{Scope: scope, ChatID: "chat", SenderID: "synthetic-operator", MessageID: "new-attachment", Body: "synthetic content", Parts: []AttachmentPart{{Kind: "file", ResourceKey: "synthetic-key", Name: "synthetic.txt"}}}
			if existingDraft {
				in.Body = ""
				batch, _, err := s.ReceiveAttachmentInput(ctx, in, 10, 5)
				if err != nil {
					t.Fatal(err)
				}
				in.MessageID, in.Body, in.ExpectedBatch, in.Parts = "fresh-explanation", "synthetic explanation", batch.ID, nil
			}
			older, _, err := s.Claim(ctx, Receipt{AppID: scope.AppID, ThreadID: scope.ThreadID, ChatID: "chat", MessageID: "older-unstaged", Body: "synthetic older input"})
			if err != nil {
				t.Fatal(err)
			}
			batch, _, err := s.ReceiveAttachmentInput(ctx, in, 10, 5)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.StageSubmissions(ctx, scope); err != nil {
				t.Fatal(err)
			}
			inputs, err := s.Submissions(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			var eligible []Submission
			for _, input := range inputs {
				if input.State != "draft" {
					eligible = append(eligible, input)
				}
			}
			if len(eligible) != 2 || eligible[0].ID != older.ID || eligible[1].ID != batch.ReceiptID || eligible[1].State != "collecting" {
				t.Fatal("attachment sealing moved an older unstaged receipt behind it")
			}
		})
	}
}
