package provider

import (
	"strings"
	"testing"
)

const (
	l1 = "2026-08-10T10:00:01.100000000Z stdout F first"
	l2 = "2026-08-10T10:00:02.200000000Z stdout F second"
	l3 = "2026-08-10T10:00:03.300000000Z stderr F third"
)

// Zun answers with the whole log every time. Following it means asking again,
// and the reason following used to be refused is that a naive reader would
// then see every line repeated at each poll.
func TestFollowEmitsEachLineExactlyOnce(t *testing.T) {
	first := []byte(strings.Join([]string{l1, l2}, "\n") + "\n")

	lines, cursor := linesAfter(first, "")
	if len(lines) != 2 {
		t.Fatalf("first read gave %d lines, want 2", len(lines))
	}
	if cursor != "2026-08-10T10:00:02.200000000Z" {
		t.Fatalf("cursor = %q", cursor)
	}

	// The same content again, as Zun would answer a second later.
	lines, cursor2 := linesAfter(first, cursor)
	if len(lines) != 0 {
		t.Errorf("re-reading the same log emitted %d lines again", len(lines))
	}
	if cursor2 != cursor {
		t.Errorf("cursor moved on unchanged input: %q -> %q", cursor, cursor2)
	}

	// Now with one more line appended.
	grown := append([]byte{}, first...)
	grown = append(grown, []byte(l3+"\n")...)
	lines, cursor3 := linesAfter(grown, cursor)
	if len(lines) != 1 || !strings.HasSuffix(string(lines[0]), "third") {
		t.Fatalf("expected only the new line, got %q", lines)
	}
	if cursor3 != "2026-08-10T10:00:03.300000000Z" {
		t.Errorf("cursor = %q", cursor3)
	}
}

// A line the runtime wrote in a shape this did not expect has no timestamp to
// place it by. Emitting it would repeat it on every poll forever, so one
// malformed line is dropped instead.
func TestALineWithNoTimestampIsNotRepeatedForever(t *testing.T) {
	raw := []byte(l1 + "\nnot a log line at all\n" + l2 + "\n")
	lines, cursor := linesAfter(raw, "")
	if len(lines) != 2 {
		t.Errorf("got %d lines, want the two that could be placed", len(lines))
	}
	if again, _ := linesAfter(raw, cursor); len(again) != 0 {
		t.Errorf("a second read emitted %d lines", len(again))
	}
}

// The timestamps are the cursor, so they are always requested; whether the
// caller sees them is decided on the way out.
func TestTimestampsAreStrippedUnlessAskedFor(t *testing.T) {
	lines, _ := linesAfter([]byte(l1+"\n"), "")

	got := string(render(lines, false))
	if got != "stdout F first\n" {
		t.Errorf("without timestamps: %q", got)
	}
	got = string(render(lines, true))
	if got != l1+"\n" {
		t.Errorf("with timestamps: %q", got)
	}
}
