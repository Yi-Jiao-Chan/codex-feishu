package setup

import (
	"context"
	"os"
	"reflect"
	"testing"

	"codex-feishu/internal/config"
	"github.com/pelletier/go-toml/v2"
)

func TestPlannerUsesInteractionDefaultsThroughApplyAndRerun(t *testing.T) {
	ctx := context.Background()
	p, in, paths := fixture(t)
	first, err := p.Build(ctx, in, paths)
	if err != nil {
		t.Fatal(err)
	}
	want := config.Default().Interactions
	if !reflect.DeepEqual(first.Config.Interactions, want) {
		t.Fatalf("new plan interactions = %#v, want %#v", first.Config.Interactions, want)
	}
	if _, err := p.Apply(ctx, in, paths, first.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Interactions, want) {
		t.Fatalf("applied interactions = %#v, want %#v", loaded.Interactions, want)
	}
	rerun, err := p.Build(ctx, in, paths)
	if err != nil {
		t.Fatal(err)
	}
	if rerun.Changed || !reflect.DeepEqual(rerun.Config.Interactions, want) {
		t.Fatalf("default rerun changed interactions: changed=%v interactions=%#v", rerun.Changed, rerun.Config.Interactions)
	}
	result, err := p.Apply(ctx, in, paths, rerun.ID)
	if err != nil || result.Changed || result.State != "configuration-unchanged" {
		t.Fatalf("default rerun apply was not a no-op: result=%+v err=%v", result, err)
	}
}

func TestPlannerPreservesExplicitInteractionSettingsOnRerun(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
		types   []string
	}{
		{name: "explicitly-off", enabled: false, types: []string{"question", "async_question"}},
		{name: "explicitly-off-with-empty-list", enabled: false, types: []string{}},
		{name: "selected-types", enabled: true, types: []string{"command", "question"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			p, in, paths := fixture(t)
			initial, err := p.Build(ctx, in, paths)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Apply(ctx, in, paths, initial.ID); err != nil {
				t.Fatal(err)
			}
			current, err := config.Load(paths.ConfigFile)
			if err != nil {
				t.Fatal(err)
			}
			current.Interactions.Enabled = test.enabled
			current.Interactions.EnabledTypes = append([]string(nil), test.types...)
			content, err := toml.Marshal(current)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.ConfigFile, content, 0600); err != nil {
				t.Fatal(err)
			}

			rerun, err := p.Build(ctx, in, paths)
			if err != nil {
				t.Fatal(err)
			}
			want := config.Default().Interactions
			want.Enabled, want.EnabledTypes = test.enabled, test.types
			if rerun.Changed || !reflect.DeepEqual(rerun.Config.Interactions, want) {
				t.Fatalf("explicit interactions were not preserved: changed=%v got=%#v want=%#v", rerun.Changed, rerun.Config.Interactions, want)
			}
		})
	}
}
