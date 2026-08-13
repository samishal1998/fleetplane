package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// Actions must survive journal round-trips exactly (invariant 7: Apply
// receives precisely what was persisted).
func TestActionJSONRoundTrip(t *testing.T) {
	in := Action{
		ActionID:    "op_01ABC",
		Kind:        "create",
		ResourceID:  "res_01DEF",
		Ref:         &ExternalRef{ID: "12345", Extra: map[string]string{"zone": "fsn1"}},
		Params:      json.RawMessage(`{"serverType":"cpx31","nested":{"a":[1,2,3]}}`),
		Destructive: false,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Action
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	// Compare via re-marshal so RawMessage formatting differences can't
	// mask real divergence.
	b2, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(b2) {
		t.Fatalf("round-trip mismatch:\n in: %s\nout: %s", b, b2)
	}
	if !reflect.DeepEqual(in.Ref, out.Ref) {
		t.Fatalf("Ref mismatch: %+v vs %+v", in.Ref, out.Ref)
	}
}

func TestClassifyUnknownErrorNeverTerminal(t *testing.T) {
	got := Classify(errors.New("connection reset by peer"))
	if got == ErrTerminal || got == ErrInvalid {
		t.Fatalf("unknown error classified %q; must stay retryable-side", got)
	}
	if got != ErrRetryable {
		t.Fatalf("Classify(unknown) = %q, want %q", got, ErrRetryable)
	}
	if Classify(context.DeadlineExceeded) != ErrRetryable {
		t.Fatal("deadline exceeded must be retryable")
	}
}

func TestEffectDefaultsToMaybe(t *testing.T) {
	// A bare transport error after send: outcome unknown.
	if Effect(errors.New("i/o timeout")) != EffectMaybe {
		t.Fatal("non-*Error must be EffectMaybe (conservative default)")
	}
	// An *Error without explicit SideEffect: still maybe.
	if Effect(&Error{Class: ErrRetryable}) != EffectMaybe {
		t.Fatal("unset SideEffect must be EffectMaybe")
	}
	// Only an explicit EffectNone allows blind retry.
	if Effect(&Error{Class: ErrInvalid, SideEffect: EffectNone}) != EffectNone {
		t.Fatal("explicit EffectNone not honored")
	}
	// Wrapped errors keep their effect.
	wrapped := fmt.Errorf("apply: %w", &Error{Class: ErrInvalid, SideEffect: EffectNone})
	if Effect(wrapped) != EffectNone {
		t.Fatal("wrapping lost SideEffect")
	}
}

func TestErrorUnwrapChain(t *testing.T) {
	cause := errors.New("root cause")
	err := fmt.Errorf("outer: %w", &Error{Class: ErrConflict, Message: "locked", Cause: cause})
	if !IsClass(err, ErrConflict) {
		t.Fatal("IsClass failed through wrapping")
	}
	if !errors.Is(err, cause) {
		t.Fatal("Unwrap chain broken")
	}
}

func TestRetryAfterOf(t *testing.T) {
	if _, ok := RetryAfterOf(errors.New("x")); ok {
		t.Fatal("plain error has no retry-after")
	}
	d, ok := RetryAfterOf(&Error{Class: ErrRateLimited, RetryAfter: 3 * time.Second})
	if !ok || d != 3*time.Second {
		t.Fatalf("RetryAfterOf = %v/%v", d, ok)
	}
}

func TestRegistryDuplicateRegisterPanics(t *testing.T) {
	f := Factory(func(context.Context, InstanceConfig) (Provider, error) { return nil, nil })
	Register("dup-test-driver", f)
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register did not panic")
		}
	}()
	Register("dup-test-driver", f)
}

func TestNewUnknownDriverIsInvalid(t *testing.T) {
	_, err := New(context.Background(), "no-such-driver", InstanceConfig{})
	if !IsClass(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if Effect(err) != EffectNone {
		t.Fatal("construction failure must be EffectNone")
	}
}

func TestIdentityLabels(t *testing.T) {
	l := IdentityLabels("owner-1", "res_x", "op_y")
	want := map[string]string{
		LabelManaged: "true", LabelOwner: "owner-1", LabelID: "res_x", LabelOp: "op_y",
	}
	if !reflect.DeepEqual(l, want) {
		t.Fatalf("IdentityLabels = %v, want %v", l, want)
	}
}
