package sync

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
)

func goalTurn(id, status string, items ...codexclient.Item) codexclient.Turn {
	return codexclient.Turn{ID: id, Status: status, ItemsView: "full", Items: items}
}

func proveGoalTurns(t *testing.T, e *Engine, allowed ...string) {
	if t != nil {
		t.Helper()
	}
	set := make(map[string]bool, len(allowed))
	for _, id := range allowed {
		set[id] = true
	}
	e.ReadGoalTurns = func(_ context.Context, thread string, turnIDs []string) (map[string]bool, error) {
		if thread != "thread" {
			if t != nil {
				t.Fatalf("goal reader received foreign thread: %q", thread)
			}
			return nil, errors.New("goal reader received foreign thread")
		}
		result := make(map[string]bool, len(turnIDs))
		for _, id := range turnIDs {
			if set[id] {
				result[id] = true
			}
		}
		return result, nil
	}
}

func assertGoalMirrorsHaveNoHumanOrParent(t *testing.T, e Engine, turnID string) {
	t.Helper()
	mirrors, err := e.Store.Mirrors(context.Background(), "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, mirror := range mirrors {
		if mirror.TurnID != turnID {
			continue
		}
		found++
		if mirror.Kind == "human" {
			t.Fatalf("goal turn acquired a human mirror: %+v", mirror)
		}
		if mirror.ParentKey != "" || mirror.ParentMessageID != "" {
			t.Fatalf("goal mirror borrowed a reply parent: %+v", mirror)
		}
		if mirror.Kind == "progress" || mirror.Kind == "final" {
			if !mirror.Direct {
				t.Fatalf("goal mirror lost its direct identity: %+v", mirror)
			}
		}
	}
	if found == 0 {
		t.Fatalf("goal turn has no public mirrors: %s", turnID)
	}
}

func TestGoalTurnProgressAndFinalSendWithoutHumanAnchor(t *testing.T) {
	e, _ := engineFixture(t)
	ordinary := completedTurn("ordinary", "ordinary-human", "ordinary request", "ordinary-final", "ordinary answer")
	goal := goalTurn("goal", "inProgress",
		agent("goal-progress", "goal is working", "commentary"),
		codexclient.Item{ID: "goal-tool", Type: "commandExecution", Status: "completed"},
	)
	proveGoalTurns(t, &e, goal.ID)

	apply(t, e, 1000, false, ordinary)
	bindEngine(t, e)
	g := &cardGateway{}
	drain(t, e, g, 2000)
	if len(g.writes) != 2 || g.writes[0].Operation != "send" || g.writes[1].Operation != "reply" || g.writes[1].Parent != g.writes[0].ID {
		t.Fatalf("ordinary turn did not establish its normal anchor: %+v", g.writes)
	}

	apply(t, e, 3000, false, ordinary, goal)
	drain(t, e, g, 3001)
	if len(g.writes) != 3 || g.writes[2].Operation != "send" || g.writes[2].Parent != "" || !strings.Contains(g.writes[2].Card, "goal is working") {
		t.Fatalf("goal progress did not send directly to the bound target: %+v", g.writes)
	}
	assertGoalMirrorsHaveNoHumanOrParent(t, e, goal.ID)
	mark, err := e.Store.Watermark(context.Background(), "host", "thread")
	if err != nil || mark.TurnID != goal.ID || mark.ItemID != "goal-tool" || mark.ObservedAtMS != 3000 {
		t.Fatalf("goal progress did not advance the complete-history watermark: %+v %v", mark, err)
	}

	goal.Status = "completed"
	goal.Items = append(goal.Items, agent("goal-final", "goal final answer", "final_answer"))
	oldWrites := len(g.writes)
	apply(t, e, 4000, false, ordinary, goal)
	drain(t, e, g, 4001)
	if len(g.writes) != oldWrites+2 {
		t.Fatalf("goal completion did not update progress and send a final: %+v", g.writes[oldWrites:])
	}
	for _, write := range g.writes[oldWrites:] {
		if strings.Contains(write.Card, "goal final answer") && (write.Operation != "send" || write.Parent != "") {
			t.Fatalf("goal final was attached to a parent: %+v", write)
		}
	}
	assertGoalMirrorsHaveNoHumanOrParent(t, e, goal.ID)
	mark, err = e.Store.Watermark(context.Background(), "host", "thread")
	if err != nil || mark.TurnID != goal.ID || mark.ItemID != "goal-final" || mark.ObservedAtMS != 4000 {
		t.Fatalf("goal final did not advance the watermark: %+v %v", mark, err)
	}

	apply(t, e, 5000, true, ordinary, goal)
	drain(t, e, g, 5001)
	if len(g.writes) != oldWrites+2 {
		t.Fatalf("goal replay duplicated direct sends: %+v", g.writes)
	}
}

func TestConsecutiveGoalsDoNotStealTheFollowingNormalAnchor(t *testing.T) {
	e, _ := engineFixture(t)
	ordinary := completedTurn("ordinary", "ordinary-human", "ordinary request", "ordinary-final", "ordinary answer")
	goalOne := goalTurn("goal-one", "completed", agent("goal-one-final", "first goal answer", "final_answer"))
	goalTwo := goalTurn("goal-two", "completed", agent("goal-two-final", "second goal answer", "final_answer"))
	followup := completedTurn("followup", "followup-human", "follow-up request", "followup-final", "follow-up answer")
	proveGoalTurns(t, &e, goalOne.ID, goalTwo.ID)

	apply(t, e, 1000, false, ordinary)
	bindEngine(t, e)
	g := &cardGateway{}
	drain(t, e, g, 2000)

	apply(t, e, 3000, false, ordinary, goalOne, goalTwo, followup)
	drain(t, e, g, 3001)
	if len(g.writes) != 6 {
		t.Fatalf("unexpected writes for consecutive goals and follow-up: %+v", g.writes)
	}
	for _, answer := range []string{"first goal answer", "second goal answer"} {
		found := false
		for _, write := range g.writes {
			if strings.Contains(write.Card, answer) {
				found = true
				if write.Operation != "send" || write.Parent != "" {
					t.Fatalf("goal answer borrowed a parent: %+v", write)
				}
			}
		}
		if !found {
			t.Fatalf("goal answer was not delivered: %s", answer)
		}
	}
	followupHuman := ""
	for _, write := range g.writes {
		if strings.Contains(write.Card, "follow-up request") {
			followupHuman = write.ID
		}
	}
	if followupHuman == "" {
		t.Fatal("follow-up human mirror was not sent")
	}
	if len(g.writes) == 0 || !strings.Contains(g.writes[len(g.writes)-1].Card, "follow-up answer") || g.writes[len(g.writes)-1].Operation != "reply" || g.writes[len(g.writes)-1].Parent != followupHuman {
		t.Fatalf("normal follow-up did not keep its own human anchor: %+v", g.writes)
	}
	assertGoalMirrorsHaveNoHumanOrParent(t, e, goalOne.ID)
	assertGoalMirrorsHaveNoHumanOrParent(t, e, goalTwo.ID)
	mark, err := e.Store.Watermark(context.Background(), "host", "thread")
	if err != nil || mark.TurnID != followup.ID || mark.ItemID != "followup-final" || mark.ObservedAtMS != 3000 {
		t.Fatalf("follow-up did not become the watermark: %+v %v", mark, err)
	}

	apply(t, e, 4000, true, ordinary, goalOne, goalTwo, followup)
	drain(t, e, g, 4001)
	if len(g.writes) != 6 {
		t.Fatalf("consecutive-goal replay duplicated sends: %+v", g.writes)
	}
}

func TestGoalSteerKeepsExistingEmptyParentsAndRequiresProofAgain(t *testing.T) {
	e, _ := engineFixture(t)
	ordinary := completedTurn("ordinary", "ordinary-human", "ordinary request", "ordinary-final", "ordinary answer")
	goal := goalTurn("goal-steer", "inProgress", agent("goal-progress", "goal before steer", "commentary"))
	readCalls := 0
	e.ReadGoalTurns = func(_ context.Context, thread string, turnIDs []string) (map[string]bool, error) {
		if thread != "thread" {
			t.Fatalf("goal reader received foreign thread: %q", thread)
		}
		readCalls++
		return map[string]bool{goal.ID: true}, nil
	}

	apply(t, e, 1000, false, ordinary)
	bindEngine(t, e)
	g := &cardGateway{}
	drain(t, e, g, 2000)
	apply(t, e, 3000, false, ordinary, goal)
	drain(t, e, g, 3001)
	if readCalls == 0 {
		t.Fatal("goal source reader was not used")
	}

	goal.Items = append(goal.Items, input("steer", "synthetic steer"), agent("goal-after-steer", "goal after steer", "final_answer"))
	oldWrites := len(g.writes)
	apply(t, e, 4000, false, ordinary, goal)
	drain(t, e, g, 4001)
	if readCalls < 2 {
		t.Fatal("goal source was not rechecked after steer")
	}
	if len(g.writes) != oldWrites+1 {
		t.Fatalf("steer should only send the newly added human input: %+v", g.writes[oldWrites:])
	}
	write := g.writes[oldWrites]
	if !strings.Contains(write.Card, "synthetic steer") || write.Operation != "send" || write.Parent != "" {
		t.Fatalf("steer human was not sent directly: %+v", write)
	}
	mirrors, err := e.Store.Mirrors(context.Background(), "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	foundSteer := false
	for _, mirror := range mirrors {
		if mirror.TurnID != goal.ID {
			continue
		}
		if mirror.Kind == "human" {
			if strings.Contains(mirror.Card, "synthetic steer") {
				foundSteer = true
			}
			continue
		}
		if (mirror.Kind == "progress" || mirror.Kind == "final") && (!mirror.Direct || mirror.ParentKey != "" || mirror.ParentMessageID != "") {
			t.Fatalf("existing goal card changed its direct empty-parent identity: %+v", mirror)
		}
	}
	if !foundSteer {
		t.Fatal("synthetic steer did not create a human mirror")
	}

	beforeMirrors, err := e.Store.Mirrors(context.Background(), "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	beforeMark, err := e.Store.Watermark(context.Background(), "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	e.ReadGoalTurns = func(context.Context, string, []string) (map[string]bool, error) {
		return map[string]bool{}, nil
	}
	if err := e.Apply(context.Background(), "thread", []codexclient.Turn{ordinary, goal}, false, 5000); err == nil {
		t.Fatal("steered goal was accepted without renewed proof")
	}
	afterMirrors, err := e.Store.Mirrors(context.Background(), "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	afterMark, err := e.Store.Watermark(context.Background(), "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterMirrors, beforeMirrors) || !reflect.DeepEqual(afterMark, beforeMark) {
		t.Fatalf("unproved steered goal changed durable state: mirrors=%+v/%+v watermark=%+v/%+v", beforeMirrors, afterMirrors, beforeMark, afterMark)
	}
}

func TestGoalProofAndSnapshotValidationAreAtomic(t *testing.T) {
	cases := []struct {
		name      string
		goal      codexclient.Turn
		configure func(*Engine)
	}{
		{
			name: "no reader",
			goal: goalTurn("goal-no-reader", "completed", agent("final", "unproved answer", "final_answer")),
		},
		{
			name: "reader does not prove turn",
			goal: goalTurn("goal-unproved", "completed", agent("final", "unproved answer", "final_answer")),
			configure: func(e *Engine) {
				e.ReadGoalTurns = func(context.Context, string, []string) (map[string]bool, error) {
					return map[string]bool{}, nil
				}
			},
		},
		{
			name: "reader error",
			goal: goalTurn("goal-reader-error", "completed", agent("final", "reader error answer", "final_answer")),
			configure: func(e *Engine) {
				e.ReadGoalTurns = func(context.Context, string, []string) (map[string]bool, error) {
					return nil, errors.New("synthetic goal reader failure")
				}
			},
		},
		{
			name: "proven summary view",
			goal: codexclient.Turn{ID: "goal-summary", Status: "completed", ItemsView: "summary", Items: []codexclient.Item{agent("final", "summary answer", "final_answer")}},
			configure: func(e *Engine) {
				proveGoalTurns(nil, e, "goal-summary")
			},
		},
		{
			name: "proven duplicate item id",
			goal: goalTurn("goal-duplicate", "completed",
				agent("duplicate", "first duplicate", "commentary"),
				agent("duplicate", "second duplicate", "final_answer"),
			),
			configure: func(e *Engine) {
				proveGoalTurns(nil, e, "goal-duplicate")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := engineFixture(t)
			base := completedTurn("base", "base-human", "base request", "base-final", "base answer")
			apply(t, e, 1000, false, base)
			beforeMirrors, err := e.Store.Mirrors(context.Background(), "host", "thread")
			if err != nil {
				t.Fatal(err)
			}
			beforeMark, err := e.Store.Watermark(context.Background(), "host", "thread")
			if err != nil {
				t.Fatal(err)
			}
			if tc.configure != nil {
				tc.configure(&e)
			}
			if err := e.Apply(context.Background(), "thread", []codexclient.Turn{base, tc.goal}, false, 2000); err == nil {
				t.Fatal("invalid or unproved goal snapshot was accepted")
			}
			afterMirrors, err := e.Store.Mirrors(context.Background(), "host", "thread")
			if err != nil {
				t.Fatal(err)
			}
			afterMark, err := e.Store.Watermark(context.Background(), "host", "thread")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(afterMirrors, beforeMirrors) || !reflect.DeepEqual(afterMark, beforeMark) {
				t.Fatalf("invalid goal snapshot partially committed: mirrors=%+v/%+v watermark=%+v/%+v", beforeMirrors, afterMirrors, beforeMark, afterMark)
			}
		})
	}
}
