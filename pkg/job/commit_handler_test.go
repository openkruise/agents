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

package job

import (
	"testing"
)

func TestGetImageSizeInfo_Found(t *testing.T) {
	output := `{"Name":"registry.example.com/team/env:v1","Size":"150.0 MiB","BlobSize":"50.0 MiB"}
{"Name":"nginx:latest","Size":"200.0 MiB","BlobSize":"80.0 MiB"}`

	info, err := getImageSizeInfo("registry.example.com/team/env:v1", output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Size != "150.0 MiB" {
		t.Errorf("Size = %s, want '150.0 MiB'", info.Size)
	}
	if info.BlobSize != "50.0 MiB" {
		t.Errorf("BlobSize = %s, want '50.0 MiB'", info.BlobSize)
	}
}

func TestGetImageSizeInfo_NotFound(t *testing.T) {
	output := `{"Name":"nginx:latest","Size":"200.0 MiB","BlobSize":"80.0 MiB"}`

	_, err := getImageSizeInfo("nonexistent:v1", output)
	if err == nil {
		t.Error("expected error for image not found")
	}
}

func TestGetImageSizeInfo_EmptyOutput(t *testing.T) {
	_, err := getImageSizeInfo("any:v1", "")
	if err == nil {
		t.Error("expected error for empty output")
	}
}

func TestGetImageSizeInfo_InvalidJSON(t *testing.T) {
	output := `not json at all
{"Name":"valid:v1","Size":"1 MiB","BlobSize":"0.5 MiB"}`

	info, err := getImageSizeInfo("valid:v1", output)
	if err != nil {
		t.Fatalf("should skip invalid lines and find valid one: %v", err)
	}
	if info.Name != "valid:v1" {
		t.Errorf("Name = %s, want 'valid:v1'", info.Name)
	}
}

func TestGetImageSizeInfo_BlankLines(t *testing.T) {
	output := `
{"Name":"img:v1","Size":"100 MiB","BlobSize":"30 MiB"}

`
	info, err := getImageSizeInfo("img:v1", output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Size != "100 MiB" {
		t.Errorf("Size = %s, want '100 MiB'", info.Size)
	}
}
