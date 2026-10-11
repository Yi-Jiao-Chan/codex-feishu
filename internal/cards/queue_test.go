package cards

import (
	"strings"
	"testing"
)

func TestQueueInputUsesOnlyVerifiedActiveTurnForSteer(t *testing.T) {
	for _, variant := range []string{"active", "stopped", "idle", "received", "unknown"} {
		t.Run(variant, func(t *testing.T) {
			v := QueueView{State: "queued", Token: "token", QueueID: "native-q", Revision: 2, QueueOrdinal: 3}
			switch variant {
			case "active":
				v.Active = true
			case "stopped":
				v.Stopped = true
			case "received":
				v.State = "received"
				v.QueueID = ""
				v.LocalPending = true
			case "unknown":
				v.State = "unknown"
			}
			card := QueueInputView(v)
			if strings.Contains(card, "追加引导") != (variant == "active") {
				t.Fatal("unverified steer")
			}
			if strings.Contains(card, "当前轮结束后执行") {
				t.Fatal("false execution promise")
			}
			if variant == "stopped" && !strings.Contains(card, "等待显式继续") {
				t.Fatal("paused queue lost guidance")
			}
			if strings.Contains(card, "cancel_pending") != (variant == "received") {
				t.Fatal("unsafe local cancel button")
			}
		})
	}
}
