package protocol

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Request{Target: "db.corp.local:5432"}); err != nil {
		t.Fatal(err)
	}
	var got Request
	if err := ReadFrame(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if got.Target != "db.corp.local:5432" {
		t.Fatalf("target = %q", got.Target)
	}
}

func TestWriteFrameRejectsOversize(t *testing.T) {
	err := WriteFrame(&bytes.Buffer{}, Request{Target: strings.Repeat("a", MaxFrame)})
	if err == nil {
		t.Fatal("oversize frame written")
	}
}

func TestReadFrameRejectsBadLengths(t *testing.T) {
	for _, n := range []uint16{0, MaxFrame + 1} {
		var hdr [2]byte
		binary.BigEndian.PutUint16(hdr[:], n)
		var r Request
		if err := ReadFrame(bytes.NewReader(hdr[:]), &r); err == nil {
			t.Errorf("length %d accepted", n)
		}
	}
}

func TestControlRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Request{Control: &Control{DrainNoticeMS: 15000}}); err != nil {
		t.Fatal(err)
	}
	var got Request
	if err := ReadFrame(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if got.Control == nil || got.Control.DrainNoticeMS != 15000 || got.Target != "" {
		t.Fatalf("got %+v", got)
	}
}
