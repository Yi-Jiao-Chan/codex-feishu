package setup

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"codex-feishu/internal/config"
)

func TestSetupCreationExplicitReplacementAndOmissionPreservation(t *testing.T) {
	p, in, paths := fixture(t)
	root := filepath.Join(filepath.Dir(paths.DatabaseFile), "creation-root")
	common := filepath.Join(root, "common")
	if err := os.MkdirAll(common, 0700); err != nil {
		t.Fatal(err)
	}
	in.Features.Phase = "phase2"
	in.Features.Creation = true
	in.Creation = &config.CreationConfig{
		Enabled:           true,
		DefaultFollowMode: "topic",
		EntryChatIDs:      []string{"oc_entry"},
		WorkgroupChatIDs:  []string{"oc_workgroup"},
		AllowedRoots:      []string{root},
		CommonDirectories: []string{common},
		TopicsEnabled:     true,
	}
	first, err := p.Build(context.Background(), in, paths)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Config.Creation.Enabled || first.Config.Creation.DefaultFollowMode != "topic" || len(first.Config.Creation.AllowedRoots) != 1 {
		t.Fatalf("explicit creation policy was not planned: %+v", first.Config.Creation)
	}
	next := strings.Join(first.NextSteps, "\n")
	for _, want := range []string{"creation scope is separate from discovery roots", "creation topics require independent real Feishu/App UI acceptance"} {
		if !strings.Contains(next, want) {
			t.Fatalf("creation next steps omitted %q: %v", want, first.NextSteps)
		}
	}
	if _, err := p.Apply(context.Background(), in, paths, first.ID); err != nil {
		t.Fatal(err)
	}

	in.Creation = nil
	in.Features.Creation = false
	preserved, err := p.Build(context.Background(), in, paths)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(preserved.Config.Creation, first.Config.Creation) {
		t.Fatalf("omitted creation replaced existing policy: got=%+v want=%+v", preserved.Config.Creation, first.Config.Creation)
	}

	in.Creation = &config.CreationConfig{Enabled: false, DefaultFollowMode: "group"}
	replaced, err := p.Build(context.Background(), in, paths)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.Config.Creation.Enabled || reflect.DeepEqual(replaced.Config.Creation, first.Config.Creation) {
		t.Fatalf("explicit creation replacement was not applied: %+v", replaced.Config.Creation)
	}
}

func TestSetupCreationFeatureRequiresExplicitObjectAndPhase2DoesNotEnable(t *testing.T) {
	p, in, paths := fixture(t)
	in.Features.Creation = true
	called := false
	p.Probe = func(context.Context, string) (Environment, error) {
		called = true
		return Environment{}, nil
	}
	if _, err := p.Build(context.Background(), in, paths); err == nil || !strings.Contains(err.Error(), "explicit creation configuration") {
		t.Fatalf("missing explicit creation object was not rejected: %v", err)
	}
	if called {
		t.Fatal("invalid creation feature reached artifact/daemon probing")
	}

	in.Features.Creation = false
	in.Features.Phase = "phase2"
	p, in, paths = fixture(t)
	in.Features.Phase = "phase2"
	plan, err := p.Build(context.Background(), in, paths)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Config.Creation.Enabled {
		t.Fatal("phase2 implicitly enabled creation")
	}
}
