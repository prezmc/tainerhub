package api

import (
	"bytes"
	"testing"
)

// A reader following along by offset sees every byte that wasn't trimmed,
// in order, while output keeps flowing past the cap.
func TestJobOutputTrimKeepsOffsets(t *testing.T) {
	j := &job{changed: make(chan struct{})}
	var want bytes.Buffer
	var got []byte
	offset := 0
	chunk := make([]byte, 10_000)
	for i := range 400 { // ~4 MB, several trims
		for k := range chunk {
			chunk[k] = byte(i + k)
		}
		j.append(chunk)
		want.Write(chunk)
		if i%7 == 0 { // read now and then, never falling behind the trim
			data, next, _, _, _ := j.read(offset)
			got = append(got, data...)
			offset = next
		}
	}
	data, next, _, _, _ := j.read(offset)
	got = append(got, data...)
	if next != want.Len() {
		t.Fatalf("next = %d, want %d", next, want.Len())
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatal("reader saw different bytes than were written")
	}
	if len(j.out) > maxJobOutput || j.dropped+len(j.out) != want.Len() {
		t.Fatalf("buffer %d bytes, dropped %d", len(j.out), j.dropped)
	}
	// A late reader from 0 gets just the retained tail.
	tail, _, _, _, _ := j.read(0)
	if !bytes.Equal(tail, want.Bytes()[j.dropped:]) {
		t.Fatal("late reader got the wrong tail")
	}
}
