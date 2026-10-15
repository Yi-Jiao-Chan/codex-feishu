package submissions

import (
	"context"
	"errors"
	"testing"

	"codex-feishu/internal/codexclient"
)

type queueStateNative struct {
	*fakeNative
	metadataReads int
}

func (*queueStateNative) Queue(context.Context, string) ([]codexclient.Queued, error) {
	return nil, errors.New("full queue must not be retained by controller")
}
func (n *queueStateNative) QueueState(ctx context.Context, thread string) ([]codexclient.Queued, error) {
	n.metadataReads++
	return n.fakeNative.Queue(ctx, thread)
}

func TestControllerUsesQueueMetadataWhenAvailable(t *testing.T) {
	c, f, _ := fixture(t, true)
	native := &queueStateNative{fakeNative: f}
	c.Native = native
	r := claim(t, c, "metadata-input")
	tick(t, c)
	if native.metadataReads < 2 || view(t, c, r.ID).State != "queued" || f.addCalls != 1 {
		t.Fatal("controller did not use bounded queue snapshots")
	}
}
