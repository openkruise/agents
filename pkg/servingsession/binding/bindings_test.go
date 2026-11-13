/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package binding

import (
	"fmt"
	"strings"
	"testing"
)

func TestDecode(t *testing.T) {
	for _, tt := range []struct {
		name    string
		raw     string
		absent  bool
		want    int
		invalid bool
	}{
		{name: "absent", absent: true},
		{name: "empty map", raw: `{}`},
		{name: "active", raw: `{"sess_1":{"state":"Active","lastRequestAt":"2026-11-03T10:00:00Z"}}`, want: 1},
		{name: "empty annotation", invalid: true},
		{name: "null", raw: `null`, invalid: true},
		{name: "invalid JSON", raw: `{`, invalid: true},
		{name: "trailing JSON", raw: `{} {}`, invalid: true},
		{name: "duplicate ID", raw: `{"s":{},"s":{}}`, invalid: true},
		{name: "unknown state", raw: `{"s":{"state":"Closing","lastRequestAt":"2026-11-03T10:00:00Z"}}`, invalid: true},
		{name: "invalid ID", raw: `{"bad.id":{"state":"Active","lastRequestAt":"2026-11-03T10:00:00Z"}}`, invalid: true},
		{name: "empty ID", raw: `{"":{"state":"Active","lastRequestAt":"2026-11-03T10:00:00Z"}}`, invalid: true},
		{name: "65 character ID", raw: `{"` + strings.Repeat("a", 65) + `":{"state":"Active","lastRequestAt":"2026-11-03T10:00:00Z"}}`, invalid: true},
		{name: "fractional seconds", raw: `{"s":{"state":"Active","lastRequestAt":"2026-11-03T10:00:00.1Z"}}`, invalid: true},
		{name: "offset", raw: `{"s":{"state":"Active","lastRequestAt":"2026-11-03T10:00:00+00:00"}}`, invalid: true},
		{name: "missing time", raw: `{"s":{"state":"Active"}}`, invalid: true},
		{name: "duplicate field", raw: `{"s":{"state":"Active","state":"Active","lastRequestAt":"2026-11-03T10:00:00Z"}}`, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			annotations := map[string]string{}
			if !tt.absent {
				annotations[BindingsAnnotation] = tt.raw
			}
			got, err := Decode(annotations)
			if (err != nil) != tt.invalid {
				t.Fatalf("Decode error=%v, invalid=%v", err, tt.invalid)
			}
			if err == nil && len(got) != tt.want {
				t.Fatalf("got %d records, want %d", len(got), tt.want)
			}
		})
	}
}

func TestEncodeKnownFieldsAndBudgets(t *testing.T) {
	input := map[string]string{BindingsAnnotation: `{"s":{"state":"Active","lastRequestAt":"2026-11-03T10:00:00Z","future":{"keep":true}}}`, "other": "kept"}
	bindings, err := Decode(input)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Encode(input, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if got["other"] != "kept" || got[BindingsAnnotation] != `{"s":{"state":"Active","lastRequestAt":"2026-11-03T10:00:00Z"}}` {
		t.Fatalf("unexpected encoded annotations: %v", got)
	}
	if input[BindingsAnnotation] != `{"s":{"state":"Active","lastRequestAt":"2026-11-03T10:00:00Z","future":{"keep":true}}}` {
		t.Fatal("mutated input")
	}
	for i := 0; i < 512; i++ {
		bindings[fmt.Sprintf("s_%062d", i)] = bindings["s"]
	}
	delete(bindings, "s")
	encoded, err := Encode(nil, bindings)
	if err != nil {
		t.Fatalf("512 records: %v", err)
	}
	if decoded, err := Decode(encoded); err != nil || len(decoded) != 512 {
		t.Fatalf("512-record round trip: count=%d error=%v", len(decoded), err)
	}
	bindings["extra"] = bindings[fmt.Sprintf("s_%062d", 0)]
	if _, err := Encode(nil, bindings); err == nil {
		t.Fatal("accepted 513 records")
	}
	delete(bindings, "extra")
	if _, err := Encode(map[string]string{"large": strings.Repeat("x", 256*1024)}, bindings); err == nil {
		t.Fatal("accepted excessive total annotations")
	}
}

func TestExactByteBudgets(t *testing.T) {
	for _, bindings := range []Bindings{nil, {}} {
		encoded, err := Encode(nil, bindings)
		if err != nil || encoded[BindingsAnnotation] != "{}" {
			t.Fatalf("empty bindings must encode as an object: %v %v", encoded, err)
		}
	}
	// Input byte limits still apply even though unknown fields are discarded.
	raw := `{"s":{"state":"Active","lastRequestAt":"2026-11-03T10:00:00Z","pad":""}}`
	padding := MaxBindingBytes - len(BindingsAnnotation) - len(raw)
	raw = strings.Replace(raw, `"pad":""`, `"pad":"`+strings.Repeat("x", padding)+`"`, 1)
	annotations := map[string]string{BindingsAnnotation: raw}
	if _, err := Decode(annotations); err != nil {
		t.Fatalf("cannot decode exact binding budget: %v", err)
	}
	annotations[BindingsAnnotation] = raw + " "
	if _, err := Decode(annotations); err == nil {
		t.Fatal("accepted binding budget plus one byte")
	}
	annotations = map[string]string{"other": strings.Repeat("x", MaxAnnotationBytes-len("other")-len(BindingsAnnotation)-len("{}"))}
	encoded, err := Encode(annotations, Bindings{})
	if err != nil {
		t.Fatalf("rejected exact annotation budget: %v", err)
	}
	if _, err := Decode(encoded); err != nil {
		t.Fatal(err)
	}
	annotations["other"] += "x"
	if _, err := Encode(annotations, Bindings{}); err == nil {
		t.Fatal("accepted annotation budget plus one byte")
	}
}
