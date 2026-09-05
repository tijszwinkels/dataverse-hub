package object

import (
	"bytes"
	"errors"
)

var ErrRevisionConflict = errors.New("revision conflict")

// SameItem ignores re-signatures and unsigned transport metadata.
func SameItem(a, b []byte) (bool, error) {
	left, _, err := ParseEnvelope(a)
	if err != nil {
		return false, err
	}
	right, _, err := ParseEnvelope(b)
	if err != nil {
		return false, err
	}
	x, err := CanonicalJSON(left.Item)
	if err != nil {
		return false, err
	}
	y, err := CanonicalJSON(right.Item)
	if err != nil {
		return false, err
	}
	return bytes.Equal(x, y), nil
}
