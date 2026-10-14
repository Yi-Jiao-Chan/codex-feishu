package codexclient

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func receiptRequest() InteractiveRequest {
	return InteractiveRequest{Kind: "question", ThreadID: "thread", TurnID: "turn", ItemID: "call_question", Questions: []InputQuestion{{ID: "layout", Header: "布局", Question: "采用哪种布局？", Other: true, Options: []InputOption{{Label: "紧凑", Description: "保留正文"}, {Label: "宽松", Description: "增加间距"}}}}}
}

func receiptRecords(t *testing.T, request InteractiveRequest) []map[string]any {
	t.Helper()
	questions := append([]InputQuestion(nil), request.Questions...)
	questions[0].Other = false // Tool handler normalizes to true in the native request.
	args, _ := json.Marshal(map[string]any{"questions": questions})
	return []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"id": request.ThreadID}},
		{"type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": request.TurnID}},
		{"type": "response_item", "payload": map[string]any{"type": "tool_search_call", "arguments": map[string]any{"query": "unrelated"}}},
		{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "request_user_input", "call_id": request.ItemID, "arguments": string(args)}},
		{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": request.ItemID, "output": `{"answers":{"layout":{"answers":["紧凑"]}}}`}},
		{"type": "event_msg", "payload": map[string]any{"type": "task_complete", "turn_id": request.TurnID}},
	}
}

func receiptLines(t *testing.T, records []map[string]any) string {
	t.Helper()
	var lines strings.Builder
	for _, record := range records {
		b, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		lines.Write(b)
		lines.WriteByte('\n')
	}
	return lines.String()
}

func TestQuestionRolloutRequiresExactIdentityAndUniqueEffectiveOutput(t *testing.T) {
	request := receiptRequest()
	for _, scenario := range []string{"valid", "historical-compaction", "wrong-session", "wrong-turn", "wrong-question", "wrong-namespace", "duplicate-call", "duplicate-output", "after-terminal", "id-less-abort", "unknown-abort", "missing-output", "partial-answer", "duplicate-json-key", "rollback", "missing-meta", "truncated-tail", "secret"} {
		t.Run(scenario, func(t *testing.T) {
			records := receiptRecords(t, request)
			r := request
			payload := func(i int) map[string]any { return records[i]["payload"].(map[string]any) }
			switch scenario {
			case "historical-compaction":
				records = append(records[:1], append([]map[string]any{{"type": "compacted", "payload": map[string]any{"message": "historical summary"}}}, records[1:]...)...)
			case "wrong-session":
				payload(0)["id"] = "another-thread"
			case "wrong-turn":
				payload(1)["turn_id"] = "another-turn"
			case "wrong-question":
				payload(3)["arguments"] = `{"questions":[{"id":"layout","header":"布局","question":"另一问题","options":[]}]}`
			case "wrong-namespace":
				payload(3)["namespace"] = "another-tool"
			case "duplicate-call":
				records = append(records, records[3])
			case "duplicate-output":
				records = append(records, records[4])
			case "after-terminal":
				records[4], records[5] = records[5], records[4]
			case "id-less-abort", "unknown-abort":
				abort := map[string]any{"type": "turn_aborted"}
				if scenario == "unknown-abort" {
					abort["turn_id"] = "unknown-turn"
				}
				records = append(records[:4], append([]map[string]any{{"type": "event_msg", "payload": abort}}, records[4:]...)...)
			case "missing-output":
				records = append(records[:4], records[5])
			case "partial-answer":
				payload(4)["output"] = `{"answers":{}}`
			case "duplicate-json-key":
				payload(4)["output"] = `{"answers":{"layout":{"answers":["紧凑"],"answers":["宽松"]}}}`
			case "rollback":
				records = append(records, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "thread_rolled_back"}})
			case "missing-meta":
				records = records[1:]
			case "secret":
				r.Questions = append([]InputQuestion(nil), r.Questions...)
				r.Questions[0].Secret = true
			}
			text := receiptLines(t, records)
			if scenario == "truncated-tail" {
				text += `{"type":"event_msg"`
			}
			result, err := readQuestionRollout(context.Background(), strings.NewReader(text), r)
			if scenario == "valid" || scenario == "historical-compaction" {
				answers, parseErr := ParseQuestionAnswers(r, result)
				if err != nil || parseErr != nil || len(answers["layout"].Answers) != 1 || answers["layout"].Answers[0] != "紧凑" {
					t.Fatal("native result not recovered", err, parseErr)
				}
			} else if !errors.Is(err, ErrQuestionResultUnavailable) || len(result) != 0 {
				t.Fatal("ambiguous answer accepted", err)
			}
		})
	}
}

func TestQuestionResultReaderOnlyOpensTrustedFilesAndRefreshesArchivePath(t *testing.T) {
	request := receiptRequest()
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	text := receiptLines(t, receiptRecords(t, request))
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	current := path
	reads := 0
	r := &QuestionResultReader{Roots: []string{root}, Metadata: func(context.Context, string) (Thread, error) {
		reads++
		return Thread{ID: request.ThreadID, Path: &current}, nil
	}}
	for i := 0; i < 2; i++ {
		if _, err := r.Read(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 1 {
		t.Fatal("unchanged metadata repeatedly requested")
	}
	changed := request
	changed.Questions = append([]InputQuestion(nil), request.Questions...)
	changed.Questions[0].Question = "不同的问题"
	if _, err := r.Read(context.Background(), changed); err == nil {
		t.Fatal("cache reused for different questions")
	}
	archived := filepath.Join(root, "archived.jsonl")
	if err := os.Rename(path, archived); err != nil {
		t.Fatal(err)
	}
	current = archived
	if _, err := r.Read(context.Background(), request); err == nil {
		t.Fatal("missing cached file was accepted")
	}
	if _, err := r.Read(context.Background(), request); err != nil || reads != 2 {
		t.Fatal("moved rollout did not refresh native metadata", err)
	}
	for _, scenario := range []string{"outside", "leaf-symlink", "directory-symlink", "writable-file"} {
		t.Run(scenario, func(t *testing.T) {
			var target string
			switch scenario {
			case "outside":
				target = filepath.Join(t.TempDir(), "outside.jsonl")
				os.WriteFile(target, []byte(text), 0600)
			case "leaf-symlink":
				target = filepath.Join(root, "symlink.jsonl")
				os.Symlink(archived, target)
			case "directory-symlink":
				symlink := filepath.Join(root, "directory")
				os.Symlink(root, symlink)
				target = filepath.Join(symlink, "archived.jsonl")
			case "writable-file":
				target = filepath.Join(root, "writable.jsonl")
				os.WriteFile(target, []byte(text), 0600)
				os.Chmod(target, 0666)
			}
			if file, err := r.open(target); err == nil {
				file.Close()
				t.Fatal("untrusted path accepted")
			}
		})
	}
}

func TestQuestionAnswerEvidencePreservesLongAppTextAndRejectsUnknownFields(t *testing.T) {
	request := receiptRequest()
	value, _ := json.Marshal(map[string]any{"answers": map[string]any{"layout": QuestionAnswer{Answers: []string{strings.Repeat("自由回答", 1000)}}}})
	if _, err := ParseQuestionAnswers(request, value); err != nil {
		t.Fatal("App answer was limited by Feishu input length", err)
	}
	for _, invalid := range []string{`{"answers":null}`, `{"answers":{"unknown":{"answers":["紧凑"]}}}`, `{"answers":{"layout":{"answers":["紧凑"],"unknown":true}}}`, `{"answers":{"layout":{"answers":[""]}}}`, `{"answers":{"layout":{"answers":["紧凑"]}},"extra":true}`} {
		if _, err := ParseQuestionAnswers(request, []byte(invalid)); err == nil {
			t.Fatal("invalid result accepted")
		}
	}
}
