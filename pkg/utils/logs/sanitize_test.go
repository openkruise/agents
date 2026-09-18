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

package logs

import "testing"

func TestSanitizeValue(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{
			name: "empty value",
		},
		{
			name:  "unchanged value",
			value: "normal value",
			want:  "normal value",
		},
		{
			name:  "newline",
			value: "before\nafter",
			want:  "beforeafter",
		},
		{
			name:  "carriage return",
			value: "before\rafter",
			want:  "beforeafter",
		},
		{
			name:  "carriage return newline",
			value: "before\r\nafter",
			want:  "beforeafter",
		},
		{
			name:  "multiple control characters",
			value: "a\nb\rc\r\nd",
			want:  "abcd",
		},
		{
			name:  "tab and unicode remain",
			value: "café\tnaïve",
			want:  "café\tnaïve",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeValue(tt.value); got != tt.want {
				t.Fatalf("SanitizeValue(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestBoundedTextCaptureSummary(t *testing.T) {
	tests := []struct {
		name      string
		edgeBytes int
		edgeWords int
		chunks    [][]byte
		want      string
	}{
		{
			name:      "empty capture",
			edgeBytes: 16,
			edgeWords: 2,
		},
		{
			name:      "zero byte budget",
			edgeWords: 2,
			chunks:    [][]byte{[]byte("not retained")},
		},
		{
			name:      "zero word budget",
			edgeBytes: 16,
			chunks:    [][]byte{[]byte("not rendered")},
		},
		{
			name:      "negative budgets produce empty summary",
			edgeBytes: -1,
			edgeWords: -1,
			chunks:    [][]byte{[]byte("not retained")},
		},
		{
			name:      "overlapping edges reconstruct complete chunked output",
			edgeBytes: 8,
			edgeWords: 10,
			chunks:    [][]byte{[]byte("hello "), []byte("world")},
			want:      "hello world",
		},
		{
			name:      "complete output is normalized to one line",
			edgeBytes: 64,
			edgeWords: 10,
			chunks:    [][]byte{[]byte("one\ntwo\r\nthree\tfour")},
			want:      "one two three four",
		},
		{
			name:      "complete output is summarized by words",
			edgeBytes: 64,
			edgeWords: 2,
			chunks:    [][]byte{[]byte("one two three four five six")},
			want:      "one two ... five six",
		},
		{
			name:      "byte budget retains both ends",
			edgeBytes: 10,
			edgeWords: 10,
			chunks:    [][]byte{[]byte("abcdefghijMID"), []byte("DLEklmnopqrst")},
			want:      "abcdefghij ... klmnopqrst",
		},
		{
			name:      "byte budget may split multibyte runes",
			edgeBytes: 4,
			edgeWords: 10,
			chunks:    [][]byte{[]byte("甲乙MID丙丁")},
			want:      "甲 ... 丁",
		},
		{
			name:      "invalid UTF-8 is removed from complete output",
			edgeBytes: 16,
			edgeWords: 4,
			chunks:    [][]byte{{'a', 0xff, ' ', 'b'}},
			want:      "a b",
		},
		{
			name:      "invalid UTF-8 is removed from bounded edges",
			edgeBytes: 2,
			edgeWords: 4,
			chunks:    [][]byte{{'a', 0xff, 'x', 'y', 0xfe, 'b'}},
			want:      "a ... b",
		},
		{
			name:      "empty chunks do not affect output",
			edgeBytes: 16,
			edgeWords: 4,
			chunks:    [][]byte{nil, []byte("kept"), {}},
			want:      "kept",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture := NewBoundedTextCapture(tt.edgeBytes, tt.edgeWords)
			for _, chunk := range tt.chunks {
				capture.Append(chunk)
			}
			if got := capture.Summary(); got != tt.want {
				t.Fatalf("Summary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBoundedTextCaptureCopiesAppendedChunks(t *testing.T) {
	capture := NewBoundedTextCapture(4, 4)
	chunk := []byte("abcdMefgh")
	capture.Append(chunk)
	copy(chunk, "XXXXXXXXX")

	if got := capture.Summary(); got != "abcd ... efgh" {
		t.Fatalf("Summary() = %q after input mutation, want %q", got, "abcd ... efgh")
	}
}

func TestBoundedTextCaptureTotalSaturates(t *testing.T) {
	t.Run("overflow flag prevents complete reconstruction", func(t *testing.T) {
		capture := NewBoundedTextCapture(4, 4)
		capture.Append([]byte("abcdef"))
		capture.totalOverflow = true

		if got := capture.Summary(); got != "abcd ... cdef" {
			t.Fatalf("Summary() = %q, want %q", got, "abcd ... cdef")
		}
	})

	t.Run("exact maximum is not overflow", func(t *testing.T) {
		capture := NewBoundedTextCapture(4, 4)
		capture.total = maxCapturedTextBytes - 1
		capture.Append([]byte("x"))

		if capture.total != maxCapturedTextBytes {
			t.Fatalf("total = %d, want %d", capture.total, maxCapturedTextBytes)
		}
		if capture.totalOverflow {
			t.Fatal("totalOverflow = true at exact maximum, want false")
		}

		capture.Append([]byte("y"))
		if capture.total != maxCapturedTextBytes {
			t.Fatalf("total after saturation = %d, want %d", capture.total, maxCapturedTextBytes)
		}
		if !capture.totalOverflow {
			t.Fatal("totalOverflow = false after exceeding maximum, want true")
		}
	})
}
