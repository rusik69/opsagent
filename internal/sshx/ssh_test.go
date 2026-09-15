package sshx

import (
	"errors"
	"strings"
	"testing"
)

func TestRunResultReturnsErrorEvenWithStdout(t *testing.T) {
	_, err := runResult(Result{Stdout: "partial output"}, errors.New("command failed"))
	if err == nil {
		t.Fatal("expected error to be returned even when stdout is present")
	}
}

func TestRunResultNilErrorReturnsResult(t *testing.T) {
	res, err := runResult(Result{Stdout: "ok", DurationMS: 5}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Stdout != "ok" || res.DurationMS != 5 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestRunResultFormatsExitError(t *testing.T) {
	_, err := runResult(Result{Stdout: "out", Stderr: "err"}, &sshExitErrorStub{status: 3})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "exited with 3") {
		t.Fatalf("expected formatted exit error, got: %v", err)
	}
}

type sshExitErrorStub struct{ status int }

func (s *sshExitErrorStub) Error() string { return "command exited" }

func (s *sshExitErrorStub) ExitStatus() int { return s.status }
