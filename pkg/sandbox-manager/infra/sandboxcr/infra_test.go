package sandboxcr

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned/fake"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	constantUtils "github.com/openkruise/agents/pkg/utils"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
)

func createTestSandbox(name, user string, phase v1alpha1.SandboxPhase, ready bool) *v1alpha1.Sandbox {
	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Annotations: map[string]string{
				v1alpha1.AnnotationOwner: user,
			},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: phase,
		},
	}

	if ready {
		sbx.Status.Conditions = []metav1.Condition{
			{
				Type:   string(v1alpha1.SandboxConditionReady),
				Status: metav1.ConditionTrue,
			},
		}
	}

	return sbx
}

//goland:noinspection GoDeprecation
func NewTestInfra(t *testing.T) (*Infra, *fake.Clientset) {
	client := fake.NewSimpleClientset()
	infraInstance, err := NewInfra(client, k8sfake.NewSimpleClientset(), proxy.NewServer(nil), constantUtils.DefaultSandboxDeployNamespace)
	assert.NoError(t, err)
	assert.NoError(t, infraInstance.Run(context.Background()))
	return infraInstance, client
}

func TestInfra_SelectSandboxes(t *testing.T) {
	utils.InitLogOutput()
	tests := []struct {
		name        string
		sandboxes   []*v1alpha1.Sandbox
		user        string
		limit       int
		filter      func(sandbox infra.Sandbox) bool
		expectNames []string
		expectCount int
	}{
		{
			name: "select all sandboxes for user",
			sandboxes: []*v1alpha1.Sandbox{
				createTestSandbox("sandbox-1", "user1", v1alpha1.SandboxRunning, true),
				createTestSandbox("sandbox-2", "user1", v1alpha1.SandboxRunning, true),
				createTestSandbox("sandbox-3", "user2", v1alpha1.SandboxRunning, true),
			},
			user:        "user1",
			limit:       10,
			expectNames: []string{"sandbox-1", "sandbox-2"},
		},
		{
			name: "select with limit",
			sandboxes: []*v1alpha1.Sandbox{
				createTestSandbox("sandbox-1", "user1", v1alpha1.SandboxRunning, true),
				createTestSandbox("sandbox-2", "user1", v1alpha1.SandboxRunning, true),
				createTestSandbox("sandbox-3", "user1", v1alpha1.SandboxRunning, true),
			},
			user:        "user1",
			limit:       2,
			expectCount: 2,
		},
		{
			name: "select with filter",
			sandboxes: []*v1alpha1.Sandbox{
				createTestSandbox("sandbox-running-1", "user1", v1alpha1.SandboxRunning, true),
				createTestSandbox("sandbox-running-2", "user1", v1alpha1.SandboxRunning, true),
			},
			user:  "user1",
			limit: 10,
			filter: func(sandbox infra.Sandbox) bool {
				return sandbox.GetName() == "sandbox-running-2"
			},
			expectNames: []string{"sandbox-running-2"},
		},
		{
			name: "select with no matching user",
			sandboxes: []*v1alpha1.Sandbox{
				createTestSandbox("sandbox-1", "user1", v1alpha1.SandboxRunning, true),
				createTestSandbox("sandbox-2", "user1", v1alpha1.SandboxRunning, true),
			},
			user:        "user2",
			limit:       10,
			expectNames: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.expectCount == 0 {
				tt.expectCount = len(tt.expectNames)
			}

			infraInstance, client := NewTestInfra(t)

			for _, sbx := range tt.sandboxes {
				CreateSandboxWithStatus(t, client, sbx)
			}
			time.Sleep(50 * time.Millisecond)

			// Test SelectSandboxes
			result, err := infraInstance.SelectSandboxes(tt.user, tt.limit, tt.filter)
			assert.NoError(t, err)
			assert.Len(t, result, tt.expectCount)
			if len(tt.expectNames) > 0 {
				var gotNames []string
				for _, sandbox := range result {
					gotNames = append(gotNames, sandbox.GetName())
				}
				assert.ElementsMatch(t, tt.expectNames, gotNames)
			}
		})
	}
}

func TestInfra_GetSandbox(t *testing.T) {
	tests := []struct {
		name        string
		sandboxes   []*v1alpha1.Sandbox
		sandboxID   string
		expectError bool
		expectFound bool
	}{
		{
			name: "get existing sandbox",
			sandboxes: []*v1alpha1.Sandbox{
				createTestSandbox("sandbox-1", "user1", v1alpha1.SandboxRunning, true),
				createTestSandbox("sandbox-2", "user2", v1alpha1.SandboxRunning, true),
			},
			sandboxID:   "default--sandbox-1",
			expectError: false,
			expectFound: true,
		},
		{
			name: "get non-existing sandbox",
			sandboxes: []*v1alpha1.Sandbox{
				createTestSandbox("sandbox-1", "user1", v1alpha1.SandboxRunning, true),
			},
			sandboxID:   "non-existent",
			expectError: true,
			expectFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infraInstance, client := NewTestInfra(t)

			// Create sandboxes
			for _, sbx := range tt.sandboxes {
				CreateSandboxWithStatus(t, client, sbx)
			}
			time.Sleep(100 * time.Millisecond)

			// Test GetSandbox
			result, err := infraInstance.GetSandbox(context.Background(), tt.sandboxID)
			if tt.expectError {
				assert.Error(t, err)
				assert.Nil(t, result)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, result)
			}
		})
	}
}

func createSandboxSets(t *testing.T, sandboxsets map[string]int32, infraInstance *Infra) {
	for name, cnt := range sandboxsets {
		for i := 0; i < int(cnt); i++ {
			namespace := fmt.Sprintf("namespace-%d", i)
			sbs := &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
			}
			_, err := infraInstance.Client.ApiV1alpha1().SandboxSets(namespace).Create(context.Background(), sbs, metav1.CreateOptions{})
			require.NoError(t, err)
		}
		require.Eventually(t, func() bool {
			return infraInstance.HasTemplate(name)
		}, 100*time.Millisecond, 5*time.Millisecond)
	}
}

func TestInfra_onSandboxSetCreate(t *testing.T) {
	tests := []struct {
		name        string
		sandboxSets map[string]int32
	}{
		{
			name: "create the first sandboxset",
			sandboxSets: map[string]int32{
				"new-sandboxset": 1,
			},
		},
		{
			name: "create multi sandboxset",
			sandboxSets: map[string]int32{
				"new-sandboxset":  1,
				"new-sandboxset2": 5,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infraInstance, _ := NewTestInfra(t)

			createSandboxSets(t, tt.sandboxSets, infraInstance)

			for name, cnt := range tt.sandboxSets {
				assert.Eventually(t, func() bool {
					actual, _ := infraInstance.templates.Load(name)
					return actual.(int32) == cnt
				}, 100*time.Millisecond, 5*time.Millisecond, fmt.Sprintf("name: %s, expect: %d", name, cnt))
			}
		})
	}
}

func TestInfra_onSandboxSetDelete(t *testing.T) {
	tests := []struct {
		name        string
		sandboxsets map[string]int32
		deleted     string
		expectCnt   int32 // 0 should be deleted
	}{
		{
			name: "delete last sbs",
			sandboxsets: map[string]int32{
				"new-sandboxset": 1,
			},
			deleted: "new-sandboxset",
		},
		{
			name: "delete non-last sbs",
			sandboxsets: map[string]int32{
				"new-sandboxset":   1,
				"new-sandboxset-2": 3,
			},
			deleted:   "new-sandboxset-2",
			expectCnt: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infraInstance, _ := NewTestInfra(t)

			createSandboxSets(t, tt.sandboxsets, infraInstance)

			// Call onSandboxSetDelete
			infraInstance.onSandboxSetDelete(&v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.deleted,
					Namespace: "namespace-0",
				},
			})

			assert.Eventually(t, func() bool {
				actual, ok := infraInstance.templates.Load(tt.deleted)
				if !ok {
					return tt.expectCnt == 0
				}
				return actual.(int32) == tt.expectCnt
			}, 100*time.Millisecond, 5*time.Millisecond)
		})
	}
}

func createTestSandboxWithDefaults(name string, namespace string) *v1alpha1.Sandbox {
	return &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:   string(v1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
				},
			},
			PodInfo: v1alpha1.PodInfo{
				PodIP: "10.0.0.1",
			},
		},
	}
}

func TestInfra_onSandboxAdd(t *testing.T) {
	tests := []struct {
		name             string
		sandbox          *v1alpha1.Sandbox
		expectRouteExist bool
	}{
		{
			name:             "add sandbox with route",
			sandbox:          createTestSandboxWithDefaults("test-sandbox", "default"),
			expectRouteExist: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infraInstance, client := NewTestInfra(t)

			// Create SandboxSet object
			sbs := &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
				},
			}

			_, err := client.ApiV1alpha1().SandboxSets("default").Create(context.Background(), sbs, metav1.CreateOptions{})
			assert.NoError(t, err)
			time.Sleep(50 * time.Millisecond)

			// Create sandbox
			tt.sandbox.Labels = map[string]string{
				v1alpha1.LabelSandboxTemplate: "test-pool",
			}
			CreateSandboxWithStatus(t, client, tt.sandbox)
			time.Sleep(50 * time.Millisecond)

			// Check route
			id := stateutils.GetSandboxID(tt.sandbox)
			route, ok := infraInstance.Proxy.LoadRoute(id)
			assert.Equal(t, tt.expectRouteExist, ok)
			if tt.expectRouteExist {
				assert.Equal(t, id, route.ID)
				assert.Equal(t, tt.sandbox.Status.PodInfo.PodIP, route.IP)
				assert.Equal(t, v1alpha1.SandboxStateRunning, route.State)
			}
		})
	}
}

func TestInfra_onSandboxDelete(t *testing.T) {
	tests := []struct {
		name             string
		sandbox          *v1alpha1.Sandbox
		addRouteFirst    bool
		expectRouteExist bool
	}{
		{
			name:             "delete sandbox with existing route",
			sandbox:          createTestSandboxWithDefaults("test-sandbox", "default"),
			expectRouteExist: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infraInstance, client := NewTestInfra(t)

			// Create sandbox
			CreateSandboxWithStatus(t, client, tt.sandbox)
			time.Sleep(50 * time.Millisecond)
			id := stateutils.GetSandboxID(tt.sandbox)

			assert.NoError(t, client.ApiV1alpha1().Sandboxes("default").Delete(context.Background(), tt.sandbox.Name, metav1.DeleteOptions{}))
			time.Sleep(10 * time.Millisecond)

			// Check route no longer exists
			_, ok := infraInstance.Proxy.LoadRoute(id)
			assert.Equal(t, tt.expectRouteExist, ok)
		})
	}
}

func TestInfra_onSandboxUpdate(t *testing.T) {
	tests := []struct {
		name              string
		oldSandbox        *v1alpha1.Sandbox
		newSandbox        *v1alpha1.Sandbox
		addTemplate       bool
		expectRouteUpdate bool
	}{
		{
			name:       "update sandbox with changed state",
			oldSandbox: createTestSandboxWithDefaults("test-sandbox", "default"),
			newSandbox: func() *v1alpha1.Sandbox {
				sbx := createTestSandboxWithDefaults("test-sandbox", "default")
				sbx.Status.Phase = v1alpha1.SandboxPaused
				return sbx
			}(),
			addTemplate:       true,
			expectRouteUpdate: true,
		},
		{
			name:              "update sandbox with unchanged state",
			oldSandbox:        createTestSandboxWithDefaults("test-sandbox", "default"),
			newSandbox:        createTestSandboxWithDefaults("test-sandbox", "default"),
			addTemplate:       true,
			expectRouteUpdate: false,
		},
		{
			name:       "update sandbox not in pool",
			oldSandbox: createTestSandboxWithDefaults("test-sandbox", "default"),
			newSandbox: func() *v1alpha1.Sandbox {
				sbx := createTestSandboxWithDefaults("test-sandbox", "default")
				sbx.Status.Phase = v1alpha1.SandboxPaused
				return sbx
			}(),
			addTemplate:       false,
			expectRouteUpdate: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infraInstance, client := NewTestInfra(t)
			// Setup pool if needed
			if tt.addTemplate {
				template := "test-pool"
				sbs := &v1alpha1.SandboxSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      template,
						Namespace: "default",
					},
				}
				_, err := client.ApiV1alpha1().SandboxSets("default").Create(context.Background(), sbs, metav1.CreateOptions{})
				require.NoError(t, err)
				require.Eventually(t, func() bool {
					return infraInstance.HasTemplate(template)
				}, 100*time.Millisecond, 5*time.Millisecond)
				// Associate sandbox with pool
				tt.newSandbox.Labels = map[string]string{
					v1alpha1.LabelSandboxTemplate: template,
				}
			}

			// Create sandbox
			CreateSandboxWithStatus(t, client, tt.oldSandbox)
			time.Sleep(10 * time.Millisecond)

			_, err := client.ApiV1alpha1().Sandboxes("default").Update(context.Background(), tt.newSandbox, metav1.UpdateOptions{})
			assert.NoError(t, err)
			time.Sleep(10 * time.Millisecond)

			// Check if route was updated
			if tt.addTemplate {
				route, ok := infraInstance.Proxy.LoadRoute(stateutils.GetSandboxID(tt.newSandbox))
				if tt.expectRouteUpdate {
					assert.True(t, ok)
					newSbx := AsSandbox(tt.newSandbox, infraInstance.Cache, infraInstance.Client)
					expectedRoute := newSbx.GetRoute()
					assert.Equal(t, expectedRoute.State, route.State)
				}
			}
		})
	}
}
