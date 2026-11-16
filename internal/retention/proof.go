// Package retention implements the existing Recall v3 challenge byte contract.
package retention

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
)

const MaxRequestBytes = 1024
const MaxRecordBytes = 2 << 20
const MaxDimensions = 65536

var ErrInvalid = errors.New("invalid retention input")
var ErrNotFound = errors.New("retention record not found")
var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func ValidID(s string) bool { return identifier.MatchString(s) }

type Request struct {
	EmbedID string `json:"embed_id"`
	Nonce   string `json:"nonce"`
}

// Decode rejects duplicate/unknown fields, trailing values and oversized bodies.
func Decode(r io.Reader) (Request, error) {
	var out Request
	b, e := io.ReadAll(io.LimitReader(r, MaxRequestBytes+1))
	if e != nil || len(b) > MaxRequestBytes {
		return out, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(b))
	t, e := d.Token()
	if e != nil || t != json.Delim('{') {
		return out, ErrInvalid
	}
	seen := map[string]bool{}
	for d.More() {
		t, e = d.Token()
		if e != nil {
			return out, ErrInvalid
		}
		k, ok := t.(string)
		if !ok || seen[k] {
			return out, ErrInvalid
		}
		seen[k] = true
		switch k {
		case "embed_id":
			e = d.Decode(&out.EmbedID)
		case "nonce":
			e = d.Decode(&out.Nonce)
		default:
			return out, ErrInvalid
		}
		if e != nil {
			return out, ErrInvalid
		}
	}
	if _, e = d.Token(); e != nil {
		return out, ErrInvalid
	}
	if _, e = d.Token(); e != io.EOF {
		return out, ErrInvalid
	}
	if !ValidID(out.EmbedID) || len(out.Nonce) != 64 {
		return out, ErrInvalid
	}
	nonce, e := hex.DecodeString(out.Nonce)
	if e != nil || len(nonce) != 32 {
		return out, ErrInvalid
	}
	return out, nil
}

type Record struct {
	Vector   []float32
	Metadata map[string]any
}

func Proof(req Request, record Record) (string, error) {
	nonce, e := hex.DecodeString(req.Nonce)
	if e != nil || len(nonce) != 32 || !ValidID(req.EmbedID) || len(record.Vector) == 0 || len(record.Vector) > MaxDimensions {
		return "", ErrInvalid
	}
	// Match the verifier's JSON round trip: metadata numbers are float64.
	raw, e := json.Marshal(record.Metadata)
	if e != nil || len(raw) > MaxRecordBytes {
		return "", ErrInvalid
	}
	var meta map[string]any
	if json.Unmarshal(raw, &meta) != nil {
		return "", ErrInvalid
	}
	rest := map[string]any{}
	var text []byte
	for k, v := range meta {
		if k == "text" {
			s, ok := v.(string)
			if !ok {
				return "", ErrInvalid
			}
			text = []byte(s)
		} else {
			rest[k] = v
		}
	}
	mj, e := json.Marshal(rest)
	if e != nil {
		return "", ErrInvalid
	}
	vec := make([]byte, 4*len(record.Vector))
	for i, f := range record.Vector {
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return "", ErrInvalid
		}
		binary.LittleEndian.PutUint32(vec[4*i:], math.Float32bits(f))
	}
	mac := hmac.New(sha256.New, nonce)
	for _, b := range [][]byte{[]byte(req.EmbedID), vec, text, mj} {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(b)))
		mac.Write(size[:])
		mac.Write(b)
	}
	return hex.EncodeToString(mac.Sum(nil)), nil
}
