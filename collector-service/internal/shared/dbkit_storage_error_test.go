package shared

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/lib/pq"
)

// Fences for bugfix 2026-09-23-collector-data-error-classified-transient:
// worker-1 re-sent one batch every 30 s for two weeks because a 2049-character
// error_message (VARCHAR(1024)) came back as SQLSTATE 22001 and the collector
// wrapped EVERY insert error as transient. Two helpers close that: ClampChars
// keeps diagnostic text inside the column, ClassifyStorageError keeps
// deterministic data errors out of the retry loop.

func TestClampChars_CountsCharactersNotBytes(t *testing.T) {
	ascii := strings.Repeat("x", 2049)
	if got := ClampChars(ascii, 1024); utf8.RuneCountInString(got) != 1024 {
		t.Fatalf("ascii: want 1024 chars, got %d", utf8.RuneCountInString(got))
	}
	// 3-byte runes: a BYTE clamp would cut mid-rune and produce invalid UTF-8.
	cjk := strings.Repeat("错", 2049)
	got := ClampChars(cjk, 1024)
	if utf8.RuneCountInString(got) != 1024 {
		t.Fatalf("cjk: want 1024 chars, got %d (bytes=%d)", utf8.RuneCountInString(got), len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("cjk: clamp split a UTF-8 sequence")
	}
	if ClampChars("short", 1024) != "short" {
		t.Fatalf("short values must pass through unchanged")
	}
	exact := strings.Repeat("y", 1024)
	if ClampChars(exact, 1024) != exact {
		t.Fatalf("a value exactly at the width must pass through unchanged")
	}
	if ClampChars("abc", 0) != "abc" {
		t.Fatalf("maxChars <= 0 must mean no clamp")
	}
}

func TestClassifyStorageError_OnlySQLStateClasses22And23AreDeterministic(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
		wantDet  bool
	}{
		{"22001 value too long", &pq.Error{Code: "22001"}, "22001", true},
		{"22021 bad encoding", &pq.Error{Code: "22021"}, "22021", true},
		{"23502 not null", &pq.Error{Code: "23502"}, "23502", true},
		{"23514 check", &pq.Error{Code: "23514"}, "23514", true},
		{"wrapped 22001", fmt.Errorf("insert ods event x: %w", &pq.Error{Code: "22001"}), "22001", true},
		{"08006 connection failure", &pq.Error{Code: "08006"}, "08006", false},
		{"40001 serialization", &pq.Error{Code: "40001"}, "40001", false},
		{"40P01 deadlock", &pq.Error{Code: "40P01"}, "40P01", false},
		{"53300 too many connections", &pq.Error{Code: "53300"}, "53300", false},
		{"57P01 admin shutdown", &pq.Error{Code: "57P01"}, "57P01", false},
		{"XX000 internal", &pq.Error{Code: "XX000"}, "XX000", false},
		{"empty code", &pq.Error{Code: ""}, "", false},
		{"driver.ErrBadConn", driver.ErrBadConn, "", false},
		{"context deadline", context.DeadlineExceeded, "", false},
		{"plain error (sqlite RAISE etc.)", errors.New("constraint failed: simulated"), "", false},
		{"nil", nil, "", false},
	}
	for _, c := range cases {
		code, det := ClassifyStorageError(c.err)
		if code != c.wantCode || det != c.wantDet {
			t.Errorf("%s: got (%q,%v) want (%q,%v)", c.name, code, det, c.wantCode, c.wantDet)
		}
	}
}

func TestWrapStorageError_TerminalOrCallersTransient(t *testing.T) {
	mk := func(e error) error { return fmt.Errorf("transient:%w", e) }
	var tde *TerminalDataError
	err := WrapStorageError(fmt.Errorf("insert: %w", &pq.Error{Code: "22001"}), mk)
	if !errors.As(err, &tde) || tde.SQLState != "22001" {
		t.Fatalf("22001 must wrap as *TerminalDataError with its SQLSTATE, got %T %v", err, err)
	}
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		t.Fatalf("TerminalDataError must keep the driver error in its chain (Unwrap)")
	}
	err = WrapStorageError(errors.New("boom"), mk)
	if errors.As(err, &tde) || !strings.HasPrefix(err.Error(), "transient:") {
		t.Fatalf("an unclassified error must go to the caller's transient wrapper, got %v", err)
	}
	err = WrapStorageError(&pq.Error{Code: "08006"}, mk)
	if errors.As(err, &tde) {
		t.Fatalf("a connection failure (08006) must stay transient — retrying it can succeed")
	}
}
