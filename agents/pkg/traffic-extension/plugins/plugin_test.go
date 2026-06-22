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

package plugins

import (
	"testing"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
)

// TestResultConstructors covers the convenience constructors that the
// plugin implementations rely on. Each constructor must return a Result
// whose Action matches its intent and whose Responses slice mirrors the
// arguments verbatim.
func TestResultConstructors(t *testing.T) {
	cont := ContinueResult()
	if cont.Action != ActionContinue || len(cont.Responses) != 0 || cont.Err != nil {
		t.Errorf("ContinueResult: %+v", cont)
	}

	rec := RecordResult()
	if rec.Action != ActionRecord || len(rec.Responses) != 0 || rec.Err != nil {
		t.Errorf("RecordResult: %+v", rec)
	}

	r := &extProcPb.ProcessingResponse{}
	mut := MutateResult(r)
	if mut.Action != ActionMutate || len(mut.Responses) != 1 || mut.Responses[0] != r {
		t.Errorf("MutateResult: %+v", mut)
	}

	imm := ImmediateResult(r, r)
	if imm.Action != ActionImmediate || len(imm.Responses) != 2 {
		t.Errorf("ImmediateResult: %+v", imm)
	}
}
