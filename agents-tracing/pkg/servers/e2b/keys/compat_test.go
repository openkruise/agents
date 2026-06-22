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

package keys

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func TestIsE2BSDKCompatible(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{name: "e2b prefix with lowercase hex", key: "e2b_abc123", want: true},
		{name: "encoded compatible key", key: expectedE2BSDKCompatibleKey("admin-987654321"), want: true},
		{name: "missing prefix", key: "abc123", want: false},
		{name: "empty suffix", key: "e2b_", want: false},
		{name: "uppercase prefix", key: "E2B_abc123", want: false},
		{name: "uppercase hex", key: "e2b_ABC123", want: false},
		{name: "non hex suffix", key: "e2b_not-hex", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsE2BSDKCompatible(tt.key))
		})
	}
}

func TestEncodeDecodeFromE2BSDKCompatible(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "legacy admin key", raw: "admin-987654321"},
		{name: "uuid key", raw: uuid.MustParse("11111111-2222-3333-4444-555555555555").String()},
		{name: "empty key", raw: ""},
		{name: "utf8 key uses byte length", raw: "key-with-\xe4\xb8\xad"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := EncodeForE2BSDK(tt.raw)

			require.Equal(t, expectedE2BSDKCompatibleKey(tt.raw), encoded)
			assert.True(t, IsE2BSDKCompatible(encoded))
			assert.True(t, strings.HasPrefix(encoded, "e2b_6f6b616701"))

			decoded, ok := DecodeFromE2BSDKCompatible(encoded)
			require.True(t, ok)
			assert.Equal(t, tt.raw, decoded)
		})
	}
}

func TestDecodeFromE2BSDKCompatibleRejectsInvalidEncoding(t *testing.T) {
	valid := EncodeForE2BSDK("raw-key")
	tests := []struct {
		name string
		key  string
	}{
		{name: "raw key", key: "raw-key"},
		{name: "sdk compatible but not openkruise encoding", key: "e2b_abc123"},
		{name: "wrong magic", key: "e2b_0000000001000000077261772d6b6579" + checksumForTest("raw-key")},
		{name: "wrong version", key: strings.Replace(valid, "01", "02", 1)},
		{name: "length mismatch", key: strings.Replace(valid, "00000007", "00000008", 1)},
		{name: "checksum mismatch", key: flipLastHexForTest(valid)},
		{name: "uppercase hex", key: strings.ToUpper(valid)},
		{name: "trailing bytes", key: valid + "00"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decoded, ok := DecodeFromE2BSDKCompatible(tt.key)
			assert.False(t, ok)
			assert.Empty(t, decoded)
		})
	}
}

func TestParseE2BSDKCompatRawLength(t *testing.T) {
	tests := []struct {
		name       string
		lengthHex  string
		wantLength int
		wantOK     bool
	}{
		{name: "valid length", lengthHex: "00000007", wantLength: 7, wantOK: true},
		{name: "length too large for int safe payload", lengthHex: "40000000", wantOK: false},
		{name: "invalid hex", lengthHex: "not-hex!", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotLength, ok := parseE2BSDKCompatRawLength(tt.lengthHex)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantLength, gotLength)
		})
	}
}

func TestToStoredRawAPIKey(t *testing.T) {
	encoded := EncodeForE2BSDK("raw-key")
	tests := []struct {
		name      string
		presented string
		wantKey   string
	}{
		{name: "raw key is unchanged", presented: "raw-key", wantKey: "raw-key"},
		{name: "encoded key returns raw key", presented: encoded, wantKey: "raw-key"},
		{name: "invalid e2b key is unchanged", presented: "e2b_abc123", wantKey: "e2b_abc123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantKey, ToStoredRawAPIKey(tt.presented))
		})
	}
}

func TestConvertToE2BCompatibleCreatedAPIKeyReturnsCopyWithCompatibleKey(t *testing.T) {
	rawKey := "raw-admin-key"
	original := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  rawKey,
		Name: "admin",
		Team: &models.Team{
			ID:   uuid.New(),
			Name: "team-a",
		},
		CreatedBy: &models.TeamUser{ID: uuid.New()},
	}

	presented := ConvertToE2BCompatibleCreatedAPIKey(original)

	require.NotNil(t, presented)
	assert.NotSame(t, original, presented)
	assert.Equal(t, rawKey, original.Key)
	assert.Equal(t, EncodeForE2BSDK(rawKey), presented.Key)
	assert.NotSame(t, original.Team, presented.Team)
	assert.NotSame(t, original.CreatedBy, presented.CreatedBy)
}

func expectedE2BSDKCompatibleKey(raw string) string {
	rawBytes := []byte(raw)
	return fmt.Sprintf("e2b_6f6b616701%08x%s%s", len(rawBytes), hex.EncodeToString(rawBytes), checksumForTest(raw))
}

func checksumForTest(raw string) string {
	payload := append([]byte("openkruise-agents/e2b-key-compat/v1"), []byte(raw)...)
	checksum := sha256.Sum256(payload)
	return hex.EncodeToString(checksum[:8])
}

func flipLastHexForTest(value string) string {
	if value[len(value)-1] == '0' {
		return value[:len(value)-1] + "1"
	}
	return value[:len(value)-1] + "0"
}

// FuzzEncodeDecodeRoundTrip verifies that encoding any raw string with EncodeForE2BSDK
// and then decoding the result with DecodeFromE2BSDKCompatible always recovers the
// original value without panicking.
func FuzzEncodeDecodeRoundTrip(f *testing.F) {
	f.Add("")
	f.Add("admin-987654321")
	f.Add("11111111-2222-3333-4444-555555555555")
	f.Add("key-with-\xe4\xb8\xad")
	f.Add(strings.Repeat("a", 1024))
	f.Add("\x00\x01\x02\xff")
	f.Add("e2b_abc123")

	f.Fuzz(func(t *testing.T, raw string) {
		encoded := EncodeForE2BSDK(raw)

		if !IsE2BSDKCompatible(encoded) {
			t.Fatalf("encoded key %q is not E2B SDK compatible", encoded)
		}

		decoded, ok := DecodeFromE2BSDKCompatible(encoded)
		if !ok {
			t.Fatalf("failed to decode encoded key for raw input %q", raw)
		}
		if decoded != raw {
			t.Fatalf("round-trip mismatch: got %q, want %q", decoded, raw)
		}
	})
}

// FuzzDecodeFromE2BSDKCompatible verifies that DecodeFromE2BSDKCompatible never panics
// on arbitrary input strings, including malformed, truncated, or random data.
func FuzzDecodeFromE2BSDKCompatible(f *testing.F) {
	f.Add("")
	f.Add("e2b_")
	f.Add("e2b_abc123")
	f.Add("e2b_6f6b616701")
	f.Add("not-a-valid-key")
	f.Add(EncodeForE2BSDK("valid-key"))
	f.Add("e2b_" + strings.Repeat("0", 200))
	f.Add("e2b_6f6b61670100000000")
	f.Add("e2b_6f6b6167010000000100" + strings.Repeat("f", 16))
	f.Add("\x00\xff\xfe")

	f.Fuzz(func(t *testing.T, input string) {
		// Must not panic regardless of input.
		decoded, ok := DecodeFromE2BSDKCompatible(input)
		if ok && decoded == "" && input != EncodeForE2BSDK("") {
			// If decode reports success with empty result, ensure it's actually the
			// encoding of empty string (legitimate case).
			reEncoded := EncodeForE2BSDK(decoded)
			if input != reEncoded {
				t.Fatalf("decode reported success for input %q but re-encode mismatch: got %q", input, reEncoded)
			}
		}
		if ok {
			// If decode succeeded, verify consistency: re-encoding the decoded value
			// and decoding again must yield the same result.
			reEncoded := EncodeForE2BSDK(decoded)
			reDecoded, reOK := DecodeFromE2BSDKCompatible(reEncoded)
			if !reOK {
				t.Fatalf("re-encode/decode failed for decoded value %q", decoded)
			}
			if reDecoded != decoded {
				t.Fatalf("re-encode/decode mismatch: got %q, want %q", reDecoded, decoded)
			}
		}
	})
}

// FuzzEncodeForE2BSDK verifies that EncodeForE2BSDK never panics on arbitrary
// input and always produces a valid E2B SDK compatible key.
func FuzzEncodeForE2BSDK(f *testing.F) {
	f.Add("")
	f.Add("simple")
	f.Add(strings.Repeat("x", 4096))
	f.Add("\x00")
	f.Add("\xff\xfe\xfd")
	f.Add("emoji-\xf0\x9f\x98\x80")
	f.Add("e2b_already_prefixed")
	f.Add("spaces and\ttabs\nnewlines")

	f.Fuzz(func(t *testing.T, raw string) {
		encoded := EncodeForE2BSDK(raw)

		if !strings.HasPrefix(encoded, "e2b_") {
			t.Fatalf("encoded key missing e2b_ prefix: %q", encoded)
		}
		if !IsE2BSDKCompatible(encoded) {
			t.Fatalf("encoded key %q failed IsE2BSDKCompatible check", encoded)
		}
	})
}
