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

package expectationutils

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/pkg/utils/expectations"
)

// resourceVersionExpectation usage:
// 1. Observes in utils.SelectObjectWithIndex, which is the final step of selecting any object from informer.
// 2. Expects when sandbox state changes, including claim, pause, resume, delete, etc.
// 3. Always check satisfied after select.
// 4. Use functions like ResourceVersionExpectationSatisfied, don't call IsSatisfied directly.
var resourceVersionExpectation = expectations.NewResourceVersionExpectation()

func ResourceVersionExpectationObserve(obj metav1.Object) {
	resourceVersionExpectation.Observe(obj)
}

func ResourceVersionExpectationExpect(obj metav1.Object) {
	resourceVersionExpectation.Expect(obj)
}

func ResourceVersionExpectationDelete(obj metav1.Object) {
	resourceVersionExpectation.Delete(obj)
}

func ResourceVersionExpectationSatisfied(obj metav1.Object) bool {
	satisfied, sinceFirstUnsatisfied := resourceVersionExpectation.IsSatisfied(obj)
	if sinceFirstUnsatisfied > expectations.ExpectationTimeout {
		ResourceVersionExpectationDelete(obj)
		return true
	}
	return satisfied
}
