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

package webhook

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/cache/controllers"
)

func TestHandlerGettersRegisterUniquePaths(t *testing.T) {
	builder, err := controllers.NewMockManagerBuilder(t)
	require.NoError(t, err)
	mgr := builder.Build()
	paths := map[string]bool{}
	for _, getter := range HandlerGetters {
		handler := getter(mgr)
		path := handler.Path()
		require.NotEmpty(t, path)
		require.False(t, paths[path], "duplicate webhook path %s", path)
		paths[path] = true
	}
	require.Contains(t, paths, "/default-sandboxset")
}
