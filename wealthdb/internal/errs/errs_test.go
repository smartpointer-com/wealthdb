package errs

import (
	"errors"
	"testing"
)

func TestExitErrorUnwrap(t *testing.T) {
	inner := errors.New("disk full")
	e := &ExitError{Code: ExitOpenFailed, Err: inner}

	if !errors.Is(e, inner) {
		t.Fatal("errors.Is(e, inner) = false, want true")
	}

	var got *ExitError
	if !errors.As(e, &got) {
		t.Fatal("errors.As(e, &got) = false, want true")
	}
	if got.Code != ExitOpenFailed {
		t.Errorf("got.Code = %d, want %d", got.Code, ExitOpenFailed)
	}
}

func TestExitErrorMessage(t *testing.T) {
	e := Newf(ExitMissingDB, "gold database %q does not exist", "/tmp/x")
	want := `gold database "/tmp/x" does not exist`
	if e.Error() != want {
		t.Errorf("Error() = %q, want %q", e.Error(), want)
	}
	if e.Code != ExitMissingDB {
		t.Errorf("Code = %d, want %d", e.Code, ExitMissingDB)
	}
}

func TestExitErrorNilSafe(t *testing.T) {
	var e *ExitError
	if e.Error() != "" {
		t.Errorf("nil.Error() = %q, want empty string", e.Error())
	}
	if e.Unwrap() != nil {
		t.Errorf("nil.Unwrap() = %v, want nil", e.Unwrap())
	}
}

func TestWrap(t *testing.T) {
	inner := errors.New("inner")
	got := Wrap(ExitSilverIO, inner)
	if got.Code != ExitSilverIO {
		t.Errorf("Code = %d, want %d", got.Code, ExitSilverIO)
	}
	if !errors.Is(got, inner) {
		t.Error("Wrap did not preserve the inner error")
	}

	if Wrap(ExitSilverIO, nil) != nil {
		t.Error("Wrap(_, nil) should return nil")
	}
}
