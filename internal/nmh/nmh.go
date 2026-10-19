// Package nmh speaks the browser's native messaging framing.
//
// Chrome, Edge and Firefox all run an external "native messaging host" over
// stdin/stdout, framing every message as a 32-bit little-endian length followed
// by UTF-8 JSON. The framing is the entire transport contract; the payload
// shape belongs to us. Nothing here reproduces IDM's own wire format - we
// designed this one.
package nmh

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// MaxOutgoing is Chrome's documented 1 MiB ceiling for a message travelling
	// host → browser. Over it the browser discards the message and closes the
	// connection without an error either side can see, so we refuse locally.
	MaxOutgoing = 1 << 20

	// MaxIncoming caps browser → host. The peer is a normal browser process,
	// but a bogus length prefix must not be able to make us allocate gigabytes
	// before noticing anything is wrong.
	MaxIncoming = 4 << 20
)

// ErrEmptyMessage reports a zero-length frame. It is always a protocol
// violation rather than end of stream, because a clean close surfaces as EOF.
var ErrEmptyMessage = errors.New("nmh: zero-length message")

// ErrTooLarge reports a frame beyond the caller's limit. The stream is then
// unusable, since the payload would still be sitting unread in the pipe.
type ErrTooLarge struct {
	Size  uint32
	Limit int
}

func (e *ErrTooLarge) Error() string {
	return fmt.Sprintf("nmh: message of %d bytes exceeds the %d byte limit", e.Size, e.Limit)
}

// Read pulls one framed message. A clean end of stream returns io.EOF, which
// callers treat as the browser going away rather than as a fault.
func Read(r io.Reader, limit int) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}

	size := binary.LittleEndian.Uint32(hdr[:])
	if size == 0 {
		return nil, ErrEmptyMessage
	}
	if uint64(size) > uint64(limit) {
		return nil, &ErrTooLarge{Size: size, Limit: limit}
	}

	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf, nil
}

// Write frames one message. Header and payload are handed to the writer as a
// single buffer: two separate writes could let a failure between them leave a
// length prefix with nothing behind it, desynchronising everything after.
func Write(w io.Writer, payload []byte, limit int) error {
	if len(payload) == 0 {
		return ErrEmptyMessage
	}
	if len(payload) > limit {
		return &ErrTooLarge{Size: uint32(len(payload)), Limit: limit}
	}

	buf := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint32(buf, uint32(len(payload)))
	copy(buf[4:], payload)

	_, err := w.Write(buf)
	return err
}

// ReadJSON reads one message and decodes it.
func ReadJSON(r io.Reader, limit int, v any) error {
	raw, err := Read(r, limit)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("nmh: decode message: %w", err)
	}
	return nil
}

// WriteJSON encodes one message and frames it.
func WriteJSON(w io.Writer, limit int, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("nmh: encode message: %w", err)
	}
	return Write(w, raw, limit)
}
