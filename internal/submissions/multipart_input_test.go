package submissions

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
)

type multipartNative struct {
	*fakeNative
	addInputs   []json.RawMessage
	steerInputs []json.RawMessage
}

func (n *multipartNative) AddInput(ctx context.Context, thread string, input json.RawMessage, client string) (codexclient.Queued, error) {
	n.addInputs = append(n.addInputs, append(json.RawMessage(nil), input...))
	return n.fakeNative.Add(ctx, thread, "multipart body", client)
}

func (n *multipartNative) SteerInput(ctx context.Context, thread, turn string, input json.RawMessage, client string) error {
	n.steerInputs = append(n.steerInputs, append(json.RawMessage(nil), input...))
	return n.fakeNative.Steer(ctx, thread, turn, "multipart body", client)
}

func setStoredInput(t *testing.T, c *Controller, receiptID, input string) {
	t.Helper()
	if _, err := c.Store.DB.Exec(`UPDATE m0_receipts SET input_json=? WHERE id=?`, input, receiptID); err != nil {
		t.Fatal(err)
	}
}

func setStoredState(t *testing.T, c *Controller, receiptID, state string) {
	t.Helper()
	if _, err := c.Store.DB.Exec(`UPDATE m0_receipts SET state=? WHERE id=?`, state, receiptID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store.DB.Exec(`UPDATE submission_inputs SET state=? WHERE receipt_id=?`, state, receiptID); err != nil {
		t.Fatal(err)
	}
}

func submissionAttemptCount(t *testing.T, c *Controller, receiptID string) int {
	t.Helper()
	rows, err := c.Store.SubmissionAttempts(context.Background(), c.scope())
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, a := range rows {
		if a.ReceiptID == receiptID {
			count++
		}
	}
	return count
}

const multipartInput = `[{"type":"text","text":"inspect attachments","text_elements":[]},{"type":"localImage","path":"/tmp/fixture-image.png"},{"type":"text","text":"report.txt: /tmp/report.txt"}]`

func TestControllerUsesFullMultipartInputForAddSteerAndRestore(t *testing.T) {
	c, f, _ := fixture(t, true)
	native := &multipartNative{fakeNative: f}
	c.Native = native
	r := claim(t, c, "multipart-all-stages")
	setStoredInput(t, c, r.ID, multipartInput)
	tick(t, c)
	if len(native.addInputs) != 1 || string(native.addInputs[0]) != multipartInput {
		t.Fatalf("add input=%q", native.addInputs)
	}
	f.afterDelete = func(f *fakeNative, _ string) {
		f.end()
		f.start("")
	}
	op := action(t, c, r.ID, "steer")
	tick(t, c)
	result, err := c.Store.SubmissionAction(context.Background(), c.scope(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Result != "restored" || len(native.steerInputs) != 1 || len(native.addInputs) != 2 {
		t.Fatalf("stages result=%q add=%d steer=%d", result.Result, len(native.addInputs), len(native.steerInputs))
	}
	if string(native.steerInputs[0]) != multipartInput || string(native.addInputs[1]) != multipartInput {
		t.Fatalf("multipart resources changed across stages: add=%q steer=%q restore=%q", native.addInputs[0], native.steerInputs[0], native.addInputs[1])
	}
}

func TestControllerRejectsMultipartBeforeAttemptWhenUnsupportedOrMalformed(t *testing.T) {
	for _, test := range []struct {
		name   string
		input  string
		native bool
		cause  error
	}{
		{name: "unsupported", input: multipartInput, cause: errInputNativeUnsupported},
		{name: "malformed", input: `[{"type":"localImage","path":"relative.png"}]`, native: true, cause: codexclient.ErrInvalidInput},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, f, _ := fixture(t, false)
			if test.native {
				c.Native = &multipartNative{fakeNative: f}
			}
			r := claim(t, c, "multipart-"+test.name)
			setStoredInput(t, c, r.ID, test.input)
			err := c.Tick(context.Background())
			if err == nil || !errors.Is(err, test.cause) || f.addCalls != 0 || submissionAttemptCount(t, c, r.ID) != 0 {
				t.Fatalf("error=%v add=%d attempts=%d", err, f.addCalls, submissionAttemptCount(t, c, r.ID))
			}
			if view(t, c, r.ID).State != "received" {
				t.Fatal("invalid multipart input changed receipt state")
			}
			if errors.Is(err, test.cause) && test.name == "malformed" && strings.Contains(err.Error(), "relative.png") {
				t.Fatal("malformed input path leaked through controller error")
			}
		})
	}
}

func TestControllerUnknownMultipartAddIsNotResent(t *testing.T) {
	c, f, _ := fixture(t, true)
	native := &multipartNative{fakeNative: f}
	c.Native = native
	r := claim(t, c, "multipart-unknown")
	setStoredInput(t, c, r.ID, multipartInput)
	f.addReplyErr = errors.New("discarded native response")
	if err := c.Tick(context.Background()); err == nil {
		t.Fatal("expected unknown add")
	}
	tick(t, c)
	if len(native.addInputs) != 1 || view(t, c, r.ID).State != "queued" {
		t.Fatalf("unknown multipart add was resent: adds=%d state=%s", len(native.addInputs), view(t, c, r.ID).State)
	}
}

func TestControllerCollectingInputBlocksOnlyLaterSubmissions(t *testing.T) {
	c, f, _ := fixture(t, false)
	first := claim(t, c, "collecting-first")
	second := claim(t, c, "collecting-second")
	if err := c.Store.StageSubmissions(context.Background(), c.scope()); err != nil {
		t.Fatal(err)
	}
	setStoredState(t, c, first.ID, "collecting")
	if err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.addCalls != 0 || view(t, c, second.ID).State != "received" {
		t.Fatalf("later input overtook collecting batch: adds=%d state=%s", f.addCalls, view(t, c, second.ID).State)
	}
	setStoredState(t, c, first.ID, "failed")
	tick(t, c)
	if f.addCalls != 1 || view(t, c, second.ID).State != "consumed" {
		t.Fatalf("failed collecting batch did not unblock later input: adds=%d state=%s", f.addCalls, view(t, c, second.ID).State)
	}
}
