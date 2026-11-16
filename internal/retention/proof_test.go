package retention

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestRequestStrictBounds(t *testing.T) {
	good := `{"embed_id":"record","nonce":"` + strings.Repeat("01", 32) + `"}`
	if _, e := Decode(strings.NewReader(good)); e != nil {
		t.Fatal(e)
	}
	for _, b := range []string{`null`, `[]`, `{}`, good + `{}`, strings.Replace(good, `"embed_id":"record"`, `"embed_id":"record","embed_id":"other"`, 1), strings.Replace(good, `"embed_id":"record"`, `"embed_id":"../other"`, 1), strings.Replace(good, `"nonce"`, `"epoch"`, 1), strings.Replace(good, strings.Repeat("01", 32), strings.Repeat("gg", 32), 1), good + strings.Repeat(" ", 1024)} {
		if _, e := Decode(strings.NewReader(b)); e == nil {
			t.Errorf("accepted invalid input length %d", len(b))
		}
	}
}
func TestProofRejectsInvalidStoredData(t *testing.T) {
	req := Request{"record", strings.Repeat("00", 32)}
	for _, r := range []Record{{}, {Vector: []float32{float32(math.NaN())}}, {Vector: []float32{float32(math.Inf(1))}}, {Vector: make([]float32, MaxDimensions+1)}, {Vector: []float32{1}, Metadata: map[string]any{"text": 5}}, {Vector: []float32{1}, Metadata: map[string]any{"text": strings.Repeat("x", MaxRecordBytes)}}} {
		if _, e := Proof(req, r); e == nil {
			t.Fatal("invalid data accepted")
		}
	}
}
func TestProofCanonicalNumberRoundtripAndNonce(t *testing.T) {
	req := Request{"record", strings.Repeat("01", 32)}
	a := Record{Vector: []float32{1}, Metadata: map[string]any{"text": "hi <&> ☃", "large": json.Number("9007199254740993")}}
	b := Record{Vector: []float32{1}, Metadata: map[string]any{"text": "hi <&> ☃", "large": float64(9007199254740993)}}
	x, e := Proof(req, a)
	if e != nil {
		t.Fatal(e)
	}
	y, _ := Proof(req, b)
	if x != y {
		t.Fatal("JSON numeric semantics differ")
	}
	req.Nonce = strings.Repeat("02", 32)
	z, _ := Proof(req, a)
	if z == x {
		t.Fatal("nonce not bound")
	}
	req.EmbedID = "other"
	z2, _ := Proof(req, a)
	if z2 == z {
		t.Fatal("record identity not bound")
	}
}
