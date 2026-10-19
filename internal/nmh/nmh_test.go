package nmh

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRoundTripOverABuffer(t *testing.T) {
	buf := new(bytes.Buffer)

	payloads := []string{`{"cmd":"ping"}`, `{"cmd":"add","url":"http://x/y"}`, ""}
	for i := range payloads {
		if payloads[i] == "" {
			payloads[i] = `{"n":` + strings.Repeat("x", i+1) + `}`
		}
		if err := Write(buf, []byte(payloads[i]), MaxIncoming); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	for i, want := range payloads {
		got, err := Read(buf, MaxIncoming)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if string(got) != want {
			t.Errorf("message %d = %q, want %q", i, got, want)
		}
	}

	if _, err := Read(buf, MaxIncoming); !errors.Is(err, io.EOF) {
		t.Errorf("after the last message: err = %v, want io.EOF", err)
	}
}

func TestRoundTripThroughAPipe(t *testing.T) {
	// A buffer hides the partial reads a real pipe produces, so the framing is
	// exercised against io.ReadFull under genuine stream semantics too.
	pr, pw := io.Pipe()

	done := make(chan error, 1)
	go func() {
		done <- Write(pw, []byte(`{"cmd":"list"}`), MaxIncoming)
	}()

	got, err := Read(pr, MaxIncoming)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != `{"cmd":"list"}` {
		t.Errorf("got %q", got)
	}
	_ = pw.Close()
	if err := <-done; err != nil {
		t.Errorf("write: %v", err)
	}
}

func TestHeaderIsLittleEndian(t *testing.T) {
	buf := new(bytes.Buffer)
	if err := Write(buf, []byte("abc"), MaxIncoming); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw := buf.Bytes()
	if len(raw) != 7 {
		t.Fatalf("frame is %d bytes, want 7", len(raw))
	}
	if le := binary.LittleEndian.Uint32(raw[:4]); le != 3 {
		t.Errorf("length prefix = %d, want 3 (little-endian)", le)
	}
	if binary.BigEndian.Uint32(raw[:4]) == 3 {
		t.Error("the prefix is also big-endian, which would be ambiguous")
	}
}

func TestOversizedIncomingIsRefusedWithoutAllocating(t *testing.T) {
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], 1<<31)

	_, err := Read(bytes.NewReader(hdr[:]), MaxIncoming)
	var tooLarge *ErrTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v, want *ErrTooLarge", err)
	}
	if tooLarge.Size != 1<<31 {
		t.Errorf("reported size = %d, want %d", tooLarge.Size, 1<<31)
	}
}

func TestOversizedOutgoingWritesNothing(t *testing.T) {
	buf := new(bytes.Buffer)
	err := Write(buf, bytes.Repeat([]byte{'z'}, MaxOutgoing+1), MaxOutgoing)
	var tooLarge *ErrTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v, want *ErrTooLarge", err)
	}
	if buf.Len() != 0 {
		t.Errorf("wrote %d bytes despite refusing the message", buf.Len())
	}
}

func TestTruncatedFramesAreReported(t *testing.T) {
	full := new(bytes.Buffer)
	if err := Write(full, []byte("0123456789"), MaxIncoming); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Run("header cut short", func(t *testing.T) {
		if _, err := Read(bytes.NewReader([]byte{0x01, 0x02}), MaxIncoming); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
		}
	})

	t.Run("payload cut short", func(t *testing.T) {
		if _, err := Read(bytes.NewReader(full.Bytes()[:6]), MaxIncoming); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
		}
	})

	t.Run("empty frame", func(t *testing.T) {
		if _, err := Read(bytes.NewReader([]byte{0, 0, 0, 0}), MaxIncoming); !errors.Is(err, ErrEmptyMessage) {
			t.Errorf("err = %v, want ErrEmptyMessage", err)
		}
	})
}

func TestJSONHelpers(t *testing.T) {
	type msg struct {
		Cmd string `json:"cmd"`
		N   int    `json:"n"`
	}

	buf := new(bytes.Buffer)
	if err := WriteJSON(buf, MaxOutgoing, msg{Cmd: "add", N: 7}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	var got msg
	if err := ReadJSON(buf, MaxIncoming, &got); err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}
	if got.Cmd != "add" || got.N != 7 {
		t.Errorf("decoded %+v, want {add 7}", got)
	}
}

func TestMalformedJSONIsReportedNotIgnored(t *testing.T) {
	buf := new(bytes.Buffer)
	if err := Write(buf, []byte("{not json"), MaxIncoming); err != nil {
		t.Fatalf("write: %v", err)
	}
	var v map[string]any
	if err := ReadJSON(buf, MaxIncoming, &v); err == nil {
		t.Fatal("malformed JSON decoded without an error")
	}
}
