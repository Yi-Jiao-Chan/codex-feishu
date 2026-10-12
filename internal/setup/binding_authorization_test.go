package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeBindingAuthorizationDefaultsAndAccepts(t *testing.T) {
	without := `{"schema_version":1}`
	in, err := Decode(strings.NewReader(without))
	if err != nil || in.Verification.BindTestGroupAndThread {
		t.Fatalf("binding authorization default changed: %+v %v", in.Verification, err)
	}

	with := `{"schema_version":1,"verification":{"bind_test_group_and_thread":true}}`
	in, err = Decode(strings.NewReader(with))
	if err != nil || !in.Verification.BindTestGroupAndThread {
		t.Fatalf("binding authorization was not decoded: %+v %v", in.Verification, err)
	}
}

func TestBindingAuthorizationChangesPlanIdentityWithoutExecutingVerification(t *testing.T) {
	ctx := context.Background()
	p, in, paths := fixture(t)
	probeCalls := 0
	probe := p.Probe
	p.Probe = func(ctx context.Context, path string) (Environment, error) {
		probeCalls++
		return probe(ctx, path)
	}

	base, err := p.Build(ctx, in, paths)
	if err != nil {
		t.Fatal(err)
	}
	in.Verification.BindTestGroupAndThread = true
	bound, err := p.Build(ctx, in, paths)
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := p.Build(ctx, in, paths)
	if err != nil {
		t.Fatal(err)
	}
	if base.ID == bound.ID || bound.ID != repeat.ID {
		t.Fatalf("binding authorization did not participate in stable plan identity: base=%s bound=%s repeat=%s", base.ID, bound.ID, repeat.ID)
	}
	if !in.Verification.BindTestGroupAndThread || !reflect.DeepEqual(bound.Verification, in.Verification) {
		t.Fatalf("plan did not preserve binding-only authorization: %#v", bound.Verification)
	}
	if in.Verification.SendTestCardToOwner || in.Verification.CreatePrivateTestGroup || in.Verification.CreateCodexTestThread {
		t.Fatalf("binding authorization enabled another verification capability: %#v", in.Verification)
	}
	if probeCalls != 3 {
		t.Fatalf("unexpected environment/verification activity count: %d", probeCalls)
	}
	for _, path := range []string{paths.ConfigFile, paths.DatabaseFile} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("preview created persistent verification/config state %q: %v", path, err)
		}
	}
	for _, path := range []string{filepath.Dir(paths.ConfigFile), filepath.Dir(paths.DatabaseFile)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("preview created a setup directory %q: %v", path, err)
		}
	}
}
