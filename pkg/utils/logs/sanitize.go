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

import (
	"math"
	"strings"
)

// SanitizeValue removes line delimiters before a value is written to a log.
func SanitizeValue(s string) string {
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")
	return s
}

// BoundedTextCapture keeps bounded excerpts from both ends of streamed text.
// It is intended for diagnostic output whose complete value may be unbounded.
//
// A BoundedTextCapture is not safe for concurrent use: Append and Summary
// mutate and read shared state without synchronization, so a single capture
// must be driven from one goroutine (or externally guarded by the caller).
type BoundedTextCapture struct {
	edgeBytes     int
	edgeWords     int
	head          []byte
	tail          []byte
	total         int64
	totalOverflow bool
}

// NewBoundedTextCapture creates a capture that keeps at most edgeBytes from
// each end and renders at most edgeWords from each end in Summary.
func NewBoundedTextCapture(edgeBytes, edgeWords int) *BoundedTextCapture {
	if edgeBytes < 0 {
		edgeBytes = 0
	}
	if edgeWords < 0 {
		edgeWords = 0
	}
	return &BoundedTextCapture{edgeBytes: edgeBytes, edgeWords: edgeWords}
}

// Append adds one streamed chunk to the capture.
func (c *BoundedTextCapture) Append(data []byte) {
	if len(data) == 0 {
		return
	}
	if remaining := c.edgeBytes - len(c.head); remaining > 0 {
		if remaining > len(data) {
			remaining = len(data)
		}
		c.head = append(c.head, data[:remaining]...)
	}
	c.tail = appendBoundedTail(c.tail, data, c.edgeBytes)
	dataLength := int64(len(data))
	if c.total > maxCapturedTextBytes-dataLength {
		c.total = maxCapturedTextBytes
		c.totalOverflow = true
	} else {
		c.total += dataLength
	}
}

// Summary renders a valid UTF-8 excerpt from the captured text.
func (c *BoundedTextCapture) Summary() string {
	if c.total == 0 || c.edgeBytes == 0 || c.edgeWords == 0 {
		return ""
	}
	// Reconstruct the complete value only when the retained edges still cover it.
	// This comparison avoids overflowing when edgeBytes is close to MaxInt.
	if !c.totalOverflow && (c.total <= int64(len(c.head)) || c.total-int64(len(c.head)) <= int64(len(c.tail))) {
		return summarizeText(string(c.completeOutput()), c.edgeWords)
	}

	headText := takeEdgeWords(string(c.head), c.edgeWords, true)
	tailText := takeEdgeWords(string(c.tail), c.edgeWords, false)
	return joinSummaryEdges(headText, tailText)
}

func (c *BoundedTextCapture) completeOutput() []byte {
	if c.total <= int64(len(c.head)) {
		return c.head[:int(c.total)]
	}
	overlap := len(c.head) + len(c.tail) - int(c.total)
	output := make([]byte, 0, len(c.head)+len(c.tail)-overlap)
	output = append(output, c.head...)
	return append(output, c.tail[overlap:]...)
}

const maxCapturedTextBytes = math.MaxInt64

func appendBoundedTail(tail, data []byte, limit int) []byte {
	if limit <= 0 {
		return tail[:0]
	}
	if len(data) >= limit {
		return append(tail[:0], data[len(data)-limit:]...)
	}
	if overflow := len(tail) + len(data) - limit; overflow > 0 {
		copy(tail, tail[overflow:])
		tail = tail[:len(tail)-overflow]
	}
	return append(tail, data...)
}

func summarizeText(text string, edgeWordCount int) string {
	text = strings.TrimSpace(strings.ToValidUTF8(text, ""))
	words := strings.Fields(text)
	if edgeWordCount <= 0 {
		return ""
	}
	// Avoid multiplying edgeWordCount so arbitrarily large public configuration
	// cannot overflow and produce an out-of-range slice.
	if edgeWordCount >= len(words) || len(words)-edgeWordCount <= edgeWordCount {
		return strings.Join(words, " ")
	}
	return strings.Join(words[:edgeWordCount], " ") + " ... " +
		strings.Join(words[len(words)-edgeWordCount:], " ")
}

func takeEdgeWords(text string, limit int, head bool) string {
	words := strings.Fields(strings.ToValidUTF8(text, ""))
	if len(words) > limit {
		if head {
			words = words[:limit]
		} else {
			words = words[len(words)-limit:]
		}
	}
	return strings.Join(words, " ")
}

func joinSummaryEdges(head, tail string) string {
	if head == "" {
		return tail
	}
	if tail == "" {
		return head
	}
	return head + " ... " + tail
}
