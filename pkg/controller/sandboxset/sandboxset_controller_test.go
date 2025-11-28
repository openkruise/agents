package sandboxset

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils/fieldindex"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var testScheme *runtime.Scheme
var codec runtime.Codec

func init() {
	testScheme = runtime.NewScheme()
	_ = v1alpha1.AddToScheme(testScheme)
	_ = corev1.AddToScheme(testScheme)
	codec = serializer.NewCodecFactory(testScheme).LegacyCodec(v1alpha1.SchemeGroupVersion)
}

var newPodKey = "is-new-pod"

func getSandboxSet(replicas int32) *v1alpha1.SandboxSet {
	// sandboxset
	sbs := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
			UID:       types.UID("123456789"),
		},
		Spec: v1alpha1.SandboxSetSpec{
			Replicas: replicas,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						newPodKey: "true",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test",
							Image: "test",
						},
					},
				},
			},
		},
	}
	return sbs
}

func getBaseSandbox(idx int32, prefix, templateHash string) *v1alpha1.Sandbox {
	return &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      prefix + strconv.Itoa(int(idx)),
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxID:    "sandbox-" + strconv.Itoa(int(idx)),
				v1alpha1.LabelTemplateHash: templateHash,
				v1alpha1.LabelSandboxPool:  "test",
			},
			Annotations: map[string]string{},
		},
	}
}

func CreateSandboxWithStatus(t *testing.T, client client.Client, sbx *v1alpha1.Sandbox) {
	ctx := context.Background()
	assert.NoError(t, client.Create(ctx, sbx))
	assert.NoError(t, client.Status().Update(ctx, sbx))
}

func NewClient() client.Client {
	return fake.NewClientBuilder().WithScheme(testScheme).
		WithStatusSubresource(&v1alpha1.SandboxSet{}, &v1alpha1.Sandbox{}).
		WithLists(&v1alpha1.SandboxSetList{}, &v1alpha1.SandboxList{}).
		WithIndex(&v1alpha1.Sandbox{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
		Build()
}

type createSandboxRequest struct {
	createCreatingSandboxes  int32
	createAvailableSandboxes int32
	createRunningSandboxes   int32
	createPausedSandboxes    int32
	createFailedSandboxes    int32
	createLegacySandboxes    int32
	createDeletedSandboxes   int32
	createKilledSandboxes    int32
	lockedOwner              string
}

func CreateSandboxes(t *testing.T, tt createSandboxRequest, sbs *v1alpha1.SandboxSet, k8sClient client.Client) int32 {
	var idx int32
	var toCreate []*v1alpha1.Sandbox
	for i := int32(0); i < tt.createCreatingSandboxes; i++ {
		sbx := getBaseSandbox(idx, "creating-", sbs.Status.UpdateRevision)
		sbx.Status.Phase = v1alpha1.SandboxPending
		sbx.Labels["type"] = "creating"
		toCreate = append(toCreate, sbx)
		idx++
	}
	for i := int32(0); i < tt.createAvailableSandboxes; i++ {
		sbx := getBaseSandbox(idx, "available-", sbs.Status.UpdateRevision)
		sbx.Status.Phase = v1alpha1.SandboxRunning
		sbx.Labels["type"] = "available"
		sbx.Labels[v1alpha1.LabelSandboxState] = v1alpha1.SandboxStateAvailable
		toCreate = append(toCreate, sbx)
		idx++
	}
	for i := int32(0); i < tt.createRunningSandboxes; i++ {
		sbx := getBaseSandbox(idx, "running-", sbs.Status.UpdateRevision)

		sbx.Status.Phase = v1alpha1.SandboxRunning
		sbx.Labels["type"] = "running"
		sbx.Labels[v1alpha1.LabelSandboxState] = v1alpha1.SandboxStateRunning
		toCreate = append(toCreate, sbx)
		idx++
	}
	for i := int32(0); i < tt.createPausedSandboxes; i++ {
		sbx := getBaseSandbox(idx, "paused-", sbs.Status.UpdateRevision)
		sbx.Status.Phase = v1alpha1.SandboxRunning
		sbx.Labels["type"] = "running"
		sbx.Labels[v1alpha1.LabelSandboxState] = v1alpha1.SandboxStatePaused
		_ = ctrl.SetControllerReference(sbs, sbx, testScheme)
		toCreate = append(toCreate, sbx)
		idx++
	}
	failedPhases := []v1alpha1.SandboxPhase{v1alpha1.SandboxFailed, v1alpha1.SandboxSucceeded}
	for i := int32(0); i < tt.createFailedSandboxes; i++ {
		sbx := getBaseSandbox(idx, "failed-", sbs.Status.UpdateRevision)
		_ = ctrl.SetControllerReference(sbs, sbx, testScheme)
		sbx.Status.Phase = failedPhases[int(idx)%len(failedPhases)]
		sbx.Labels["type"] = "failed"
		toCreate = append(toCreate, sbx)
		idx++
	}
	for i := int32(0); i < tt.createLegacySandboxes; i++ {
		sbx := getBaseSandbox(idx, "legacy-", "legacy-hash")
		_ = ctrl.SetControllerReference(sbs, sbx, testScheme)
		sbx.Status.Phase = v1alpha1.SandboxRunning
		sbx.Labels["type"] = "legacy"
		sbx.Labels[v1alpha1.LabelSandboxState] = v1alpha1.SandboxStateRunning
		toCreate = append(toCreate, sbx)
		idx++
	}
	for i := int32(0); i < tt.createDeletedSandboxes; i++ {
		sbx := getBaseSandbox(idx, "deleted-", sbs.Status.UpdateRevision)
		_ = ctrl.SetControllerReference(sbs, sbx, testScheme)
		sbx.Status.Phase = v1alpha1.SandboxRunning
		sbx.Labels["type"] = "deleted"
		sbx.Finalizers = []string{"kruise.test/finalizer"}
		toCreate = append(toCreate, sbx)
		idx++
	}
	for i := int32(0); i < tt.createKilledSandboxes; i++ {
		sbx := getBaseSandbox(idx, "killed-", sbs.Status.UpdateRevision)
		_ = ctrl.SetControllerReference(sbs, sbx, testScheme)
		killPerformed := idx%2 > 0
		if killPerformed {
			sbx.Status.Phase = v1alpha1.SandboxTerminating
		} else {
			sbx.Status.Phase = v1alpha1.SandboxRunning
			sbx.Labels["type"] = "killed"
			sbx.Labels[v1alpha1.LabelSandboxState] = v1alpha1.SandboxStateKilling
		}
		toCreate = append(toCreate, sbx)
		idx++
	}
	for _, sbx := range toCreate {
		_ = ctrl.SetControllerReference(sbs, sbx, testScheme)
		if tt.lockedOwner != "" {
			sbx.Annotations[v1alpha1.AnnotationLock] = "some-lock"
			sbx.Annotations[v1alpha1.AnnotationOwner] = tt.lockedOwner
		}
		CreateSandboxWithStatus(t, k8sClient, sbx)
		if strings.HasPrefix(sbx.Name, "deleted-") {
			assert.NoError(t, k8sClient.Delete(context.TODO(), sbx))
		}
	}
	return idx
}

func CheckAllEvents(t *testing.T, eventRecorder *record.FakeRecorder, expectEvents []string) {
	for _, expectedEvent := range expectEvents {
		CheckEvent(t, eventRecorder, corev1.EventTypeNormal, expectedEvent)
	}
	select {
	case event := <-eventRecorder.Events:
		t.Errorf("unexpected event: %s", event)
	default:
	}
}

func CheckEvent(t *testing.T, eventRecorder *record.FakeRecorder, tp, evt string) {
	select {
	case event := <-eventRecorder.Events:
		t.Log(event)
		prefix := fmt.Sprintf("%s %s", tp, evt)
		assert.Equal(t, prefix, event[:len(prefix)])
	default:
		t.Errorf("no event received")
	}
}

func TestReconcile_ReleaseControl(t *testing.T) {
	utils.InitLogOutput()
	checkFunc := func(expectReleasedCnt int) func(t *testing.T, client client.Client, sbs *v1alpha1.SandboxSet) {
		return func(t *testing.T, client client.Client, sbs *v1alpha1.SandboxSet) {
			var sandboxList v1alpha1.SandboxList
			assert.NoError(t, client.List(context.Background(), &sandboxList))

			releasedCount := len(sandboxList.Items)
			for _, sbx := range sandboxList.Items {
				if sbx.Labels[newPodKey] != "" {
					releasedCount--
					continue
				}
				owners := fieldindex.OwnerIndexFunc(&sbx)
				for _, owner := range owners {
					if owner == string(sbs.UID) {
						releasedCount--
					}
				}
			}
			assert.Equal(t, expectReleasedCnt, releasedCount)
		}
	}
	tests := []struct {
		name         string
		request      createSandboxRequest
		replicas     int32
		expectEvents []string
		checkFunc    func(t *testing.T, client client.Client, sbs *v1alpha1.SandboxSet)
	}{
		{
			name: "not released",
			request: createSandboxRequest{
				createAvailableSandboxes: 2,
			},
			replicas:  2,
			checkFunc: checkFunc(0),
		},
		{
			name: "release running",
			request: createSandboxRequest{
				createAvailableSandboxes: 1,
				createRunningSandboxes:   1,
			},
			replicas:     1,
			expectEvents: []string{EventSandboxReleased},
			checkFunc:    checkFunc(1),
		},
		{
			name: "release running and paused",
			request: createSandboxRequest{
				createPausedSandboxes:  1,
				createRunningSandboxes: 1,
			},
			replicas:     0,
			expectEvents: []string{EventSandboxReleased, EventSandboxReleased},
			checkFunc:    checkFunc(2),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			k8sClient := NewClient()

			sbs := getSandboxSet(tt.replicas)
			assert.NoError(t, k8sClient.Create(ctx, sbs))

			eventRecorder := record.NewFakeRecorder(10)
			reconciler := &Reconciler{
				Client:   k8sClient,
				Scheme:   testScheme,
				Recorder: eventRecorder,
				Codec:    codec,
			}
			newStatus, err := reconciler.initNewStatus(sbs)
			assert.NoError(t, err)
			sbs.Status = *newStatus
			CreateSandboxes(t, tt.request, sbs, k8sClient)

			_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sbs)})
			assert.NoError(t, err)

			if tt.checkFunc != nil {
				tt.checkFunc(t, k8sClient, sbs)
			}
			CheckAllEvents(t, eventRecorder, tt.expectEvents)
		})
	}
}

func TestReconcile_ProcessCreating(t *testing.T) {
	utils.InitLogOutput()
	checkFunc := func(expectCnt int) func(t *testing.T, client client.Client, sbs *v1alpha1.SandboxSet) {
		return func(t *testing.T, client client.Client, sbs *v1alpha1.SandboxSet) {
			var sandboxList v1alpha1.SandboxList
			assert.NoError(t, client.List(context.Background(), &sandboxList))

			availableCount := 0
			for _, sbx := range sandboxList.Items {
				if sbx.Labels[v1alpha1.LabelSandboxState] == v1alpha1.SandboxStateAvailable {
					availableCount++
				}
			}
			assert.Equal(t, expectCnt, availableCount, "Expected no sandboxes to become available")
		}
	}
	tests := []struct {
		name         string
		makeReady    bool // 是否将Creating状态的沙箱标记为就绪
		expectEvents []string
		checkFunc    func(t *testing.T, client client.Client, sbs *v1alpha1.SandboxSet)
	}{
		{
			name:         "process ready creating sandboxes",
			makeReady:    true,
			expectEvents: []string{EventSandboxAvailable, EventSandboxAvailable},
			checkFunc:    checkFunc(2),
		},
		{
			name:         "process non-ready creating sandboxes",
			makeReady:    false,
			expectEvents: []string{},
			checkFunc:    checkFunc(0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			k8sClient := NewClient()

			sbs := getSandboxSet(2)
			assert.NoError(t, k8sClient.Create(ctx, sbs))

			eventRecorder := record.NewFakeRecorder(10)
			reconciler := &Reconciler{
				Client:   k8sClient,
				Scheme:   testScheme,
				Recorder: eventRecorder,
				Codec:    codec,
			}

			// 创建沙箱
			newStatus, err := reconciler.initNewStatus(sbs)
			assert.NoError(t, err)
			sbs.Status = *newStatus
			CreateSandboxes(t, createSandboxRequest{createCreatingSandboxes: 2}, sbs, k8sClient)

			// 如果需要，将Creating状态的沙箱标记为就绪
			if tt.makeReady {
				var sandboxList v1alpha1.SandboxList
				assert.NoError(t, k8sClient.List(ctx, &sandboxList))
				for i := range sandboxList.Items {
					sbx := &sandboxList.Items[i]
					if sbx.Labels["type"] == "creating" {
						// 添加Ready条件
						sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						})
						assert.NoError(t, k8sClient.Status().Update(ctx, sbx))
					}
				}
			}

			_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sbs)})
			assert.NoError(t, err)
			if tt.checkFunc != nil {
				tt.checkFunc(t, k8sClient, sbs)
			}
			CheckAllEvents(t, eventRecorder, tt.expectEvents)
		})
	}
}

func TestReconcile_DeleteFailed(t *testing.T) {
	utils.InitLogOutput()
	checkFunc := func(expectNonDeletedCnt int) func(t *testing.T, client client.Client, sbs *v1alpha1.SandboxSet) {
		return func(t *testing.T, client client.Client, sbs *v1alpha1.SandboxSet) {
			var sandboxList v1alpha1.SandboxList
			assert.NoError(t, client.List(context.Background(), &sandboxList))

			nonDeletedCount := 0
			for _, sbx := range sandboxList.Items {
				if sbx.DeletionTimestamp == nil {
					nonDeletedCount++
				}
			}
			assert.Equal(t, expectNonDeletedCnt, nonDeletedCount, "Expected 1 sandbox to remain (available)")
		}
	}
	tests := []struct {
		name         string
		request      createSandboxRequest
		replicas     int32
		expectEvents []string
		checkFunc    func(t *testing.T, client client.Client, sbs *v1alpha1.SandboxSet)
	}{
		{
			name: "delete failed sandboxes",
			request: createSandboxRequest{
				createFailedSandboxes:    2,
				createAvailableSandboxes: 1,
			},
			replicas:     1,
			expectEvents: []string{EventFailedSandboxDeleted, EventFailedSandboxDeleted},
			checkFunc:    checkFunc(1),
		},
		{
			name: "delete failed sandboxes with already deleted ones",
			request: createSandboxRequest{
				createFailedSandboxes:    2,
				createDeletedSandboxes:   1, // will not send a event
				createAvailableSandboxes: 1,
			},
			replicas:     1,
			expectEvents: []string{EventFailedSandboxDeleted, EventFailedSandboxDeleted},
			checkFunc:    checkFunc(1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			k8sClient := NewClient()

			sbs := getSandboxSet(tt.replicas)
			eventRecorder := record.NewFakeRecorder(10)
			reconciler := &Reconciler{
				Client:   k8sClient,
				Scheme:   testScheme,
				Recorder: eventRecorder,
				Codec:    codec,
			}
			assert.NoError(t, k8sClient.Create(ctx, sbs))
			newStatus, err := reconciler.initNewStatus(sbs)

			assert.NoError(t, err)
			sbs.Status = *newStatus
			CreateSandboxes(t, tt.request, sbs, k8sClient)

			_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sbs)})
			assert.NoError(t, err)

			if tt.checkFunc != nil {
				tt.checkFunc(t, k8sClient, sbs)
			}
			CheckAllEvents(t, eventRecorder, tt.expectEvents)
		})
	}
}

func TestReconcile_BasicScale(t *testing.T) {
	utils.InitLogOutput()
	checkFunc := func(totCnt, newCnt int) func(t *testing.T, client client.Client, sbs *v1alpha1.SandboxSet) {
		return func(t *testing.T, client client.Client, sbs *v1alpha1.SandboxSet) {
			var sandboxList v1alpha1.SandboxList
			assert.NoError(t, client.List(context.Background(), &sandboxList))

			var gotTotal, gotNew int
			for _, sbx := range sandboxList.Items {
				if sbx.DeletionTimestamp == nil {
					gotTotal++
				}
				if sbx.Labels[newPodKey] == "true" {
					gotNew++
				}
			}
			assert.Equal(t, totCnt, gotTotal)
			assert.Equal(t, newCnt, gotNew)
		}
	}
	tests := []struct {
		name     string
		replicas int32

		// create sandboxes before reconcile
		request createSandboxRequest

		// expect results after reconcile
		expectTotalSandboxes int
		expectNewSandboxes   int
		expectEvents         []string
	}{
		// Default watermarks are used in all cases: high = 80%, low = 60%
		{
			name:                 "simple scale up from 0",
			replicas:             2,
			expectTotalSandboxes: 2,
			expectNewSandboxes:   2,
			expectEvents:         []string{EventSandboxCreated, EventSandboxCreated},
		},
		{
			name:     "1 claimed, scale up from 1 to 2",
			replicas: 2,
			request: createSandboxRequest{
				createAvailableSandboxes: 1,
				createRunningSandboxes:   1,
			},
			expectTotalSandboxes: 3,
			expectNewSandboxes:   1,
			expectEvents: []string{
				EventSandboxReleased,
				EventSandboxCreated,
			},
		},
		{
			name:     "2 available, 1 running, not scale",
			replicas: 2,
			request: createSandboxRequest{
				createAvailableSandboxes: 2,
				createRunningSandboxes:   1,
			},
			expectTotalSandboxes: 3,
			expectNewSandboxes:   0,
			expectEvents: []string{
				EventSandboxReleased,
			},
		},
		{
			name:     "2 running, 2 paused",
			replicas: 2,
			request: createSandboxRequest{
				createRunningSandboxes: 2,
				createPausedSandboxes:  2,
			},
			expectTotalSandboxes: 6,
			expectNewSandboxes:   2,
			expectEvents: []string{
				EventSandboxReleased, EventSandboxReleased, EventSandboxReleased, EventSandboxReleased,
				EventSandboxCreated, EventSandboxCreated,
			},
		},
		{
			name:     "1 deleted, scale up from 1 to 2",
			replicas: 2,
			request: createSandboxRequest{
				createDeletedSandboxes:   1,
				createAvailableSandboxes: 1,
			},
			expectTotalSandboxes: 2,
			expectNewSandboxes:   1,
			expectEvents:         []string{EventSandboxCreated},
		},
		{
			name:     "1 killed, scale up from 1 to 2, 1 gc",
			replicas: 2,
			request: createSandboxRequest{
				createKilledSandboxes:    1,
				createAvailableSandboxes: 1,
			},
			expectTotalSandboxes: 2,
			expectNewSandboxes:   1,
			expectEvents:         []string{EventFailedSandboxDeleted, EventSandboxCreated},
		},
		{
			name:     "scale down 1 available",
			replicas: 2,
			request: createSandboxRequest{
				createAvailableSandboxes: 3,
			},
			expectTotalSandboxes: 2,
			expectEvents:         []string{EventSandboxScaledDown},
		},
		{
			name:     "scale down 1 creating",
			replicas: 2,
			request: createSandboxRequest{
				createCreatingSandboxes:  1,
				createAvailableSandboxes: 2,
			},
			expectTotalSandboxes: 2,
			expectEvents:         []string{EventSandboxScaledDown},
		},
		{
			name:     "complex",
			replicas: 3,
			request: createSandboxRequest{
				createAvailableSandboxes: 2,
				createRunningSandboxes:   2,
				createPausedSandboxes:    2,
				createFailedSandboxes:    2, // should gc
				createKilledSandboxes:    2, // should gc
				createDeletedSandboxes:   2, // should gc
			},
			expectEvents: []string{
				EventSandboxReleased, EventSandboxReleased,
				EventSandboxReleased, EventSandboxReleased,
				EventFailedSandboxDeleted, EventFailedSandboxDeleted,
				EventFailedSandboxDeleted, EventFailedSandboxDeleted,
				EventSandboxCreated,
			},
			expectTotalSandboxes: 7,
			expectNewSandboxes:   1,
		},
		{
			name:     "user story 1, step 1: claim one from init",
			replicas: 2,
			request: createSandboxRequest{
				createAvailableSandboxes: 1,
				createRunningSandboxes:   1,
			},
			expectTotalSandboxes: 3,
			expectNewSandboxes:   1,
			expectEvents:         []string{EventSandboxReleased, EventSandboxCreated},
		},
		{
			name:     "user story 1, step 2: pause it",
			replicas: 2,
			request: createSandboxRequest{
				createAvailableSandboxes: 2,
				createPausedSandboxes:    1,
			},
			expectTotalSandboxes: 3,
			expectEvents:         []string{EventSandboxReleased},
		},
		{
			name:     "user story 1, step 3: claim the second",
			replicas: 2,
			request: createSandboxRequest{
				createAvailableSandboxes: 1,
				createRunningSandboxes:   1,
				createPausedSandboxes:    1,
			},
			expectTotalSandboxes: 4,
			expectNewSandboxes:   1,
			expectEvents:         []string{EventSandboxReleased, EventSandboxReleased, EventSandboxCreated},
		},
		{
			name:     "user story 1, step 4: claim the third",
			replicas: 2,
			request: createSandboxRequest{
				createAvailableSandboxes: 1,
				createRunningSandboxes:   2,
				createPausedSandboxes:    1,
			},
			expectTotalSandboxes: 5,
			expectNewSandboxes:   1,
			expectEvents:         []string{EventSandboxReleased, EventSandboxReleased, EventSandboxReleased, EventSandboxCreated},
		},
		{
			name:     "user story 1, step 5: claim the forth",
			replicas: 2,
			request: createSandboxRequest{
				createAvailableSandboxes: 1,
				createRunningSandboxes:   3,
				createPausedSandboxes:    1,
			},
			expectTotalSandboxes: 6,
			expectNewSandboxes:   1,
			expectEvents:         []string{EventSandboxReleased, EventSandboxReleased, EventSandboxReleased, EventSandboxReleased, EventSandboxCreated},
		},
		{
			name:     "user story 1, step 6: kill the first",
			replicas: 2,
			request: createSandboxRequest{
				createAvailableSandboxes: 2,
				createRunningSandboxes:   2,
				createPausedSandboxes:    1,
			},
			expectTotalSandboxes: 5,
			expectEvents:         []string{EventSandboxReleased, EventSandboxReleased, EventSandboxReleased},
		},
		{
			name:     "user story 1, step 7: kill the second and third and the forth",
			replicas: 2,
			request: createSandboxRequest{
				createAvailableSandboxes: 2,
				createPausedSandboxes:    1,
			},
			expectEvents:         []string{EventSandboxReleased},
			expectTotalSandboxes: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			k8sClient := NewClient()

			sbs := getSandboxSet(tt.replicas)
			assert.NoError(t, k8sClient.Create(ctx, sbs))

			eventRecorder := record.NewFakeRecorder(10)
			reconciler := &Reconciler{
				Client:   k8sClient,
				Scheme:   testScheme,
				Recorder: eventRecorder,
				Codec:    codec,
			}
			newStatus, err := reconciler.initNewStatus(sbs)

			assert.NoError(t, err)
			sbs.Status = *newStatus
			_ = CreateSandboxes(t, tt.request, sbs, k8sClient)
			scaleExpectation.DeleteExpectations(GetControllerKey(sbs))
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: client.ObjectKeyFromObject(sbs),
			})
			assert.NoError(t, err)
			checkFunc(tt.expectTotalSandboxes, tt.expectNewSandboxes)(t, k8sClient, sbs)

			var gotSbs v1alpha1.SandboxSet
			assert.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(sbs), &gotSbs))

			status := gotSbs.Status
			assert.Equal(t, tt.replicas, status.Replicas)
			assert.Equal(t, tt.request.createAvailableSandboxes, status.AvailableReplicas)

			CheckAllEvents(t, eventRecorder, tt.expectEvents)
		})
	}
}

func TestReconcile_ScaleDown(t *testing.T) {
	utils.InitLogOutput()
	tests := []struct {
		name         string
		replicas     int32
		request      createSandboxRequest
		expectEvents []string
		checkFunc    func(t *testing.T, sandboxes []v1alpha1.Sandbox)
	}{
		{
			name:     "scale down available sandboxes",
			replicas: 0,
			request: createSandboxRequest{
				createAvailableSandboxes: 1,
			},
			expectEvents: []string{EventSandboxScaledDown},
			checkFunc: func(t *testing.T, sandboxes []v1alpha1.Sandbox) {
				assert.Equal(t, 0, len(sandboxes))
			},
		},
		{
			name:     "scale down creating sandboxes",
			replicas: 0,
			request: createSandboxRequest{
				createCreatingSandboxes: 1,
			},
			expectEvents: []string{EventSandboxScaledDown},
			checkFunc: func(t *testing.T, sandboxes []v1alpha1.Sandbox) {
				assert.Equal(t, 0, len(sandboxes))
			},
		},
		{
			name:     "not delete running sandboxes",
			replicas: 0,
			request: createSandboxRequest{
				createRunningSandboxes: 1,
			},
			checkFunc: func(t *testing.T, sandboxes []v1alpha1.Sandbox) {
				assert.Equal(t, 1, len(sandboxes))
			},
			expectEvents: []string{EventSandboxReleased},
		},
		{
			name:     "scale down mixed sandboxes (creating first)",
			replicas: 1,
			request: createSandboxRequest{
				createAvailableSandboxes: 2,
				createCreatingSandboxes:  2,
			},
			expectEvents: []string{EventSandboxScaledDown, EventSandboxScaledDown, EventSandboxScaledDown},
			checkFunc: func(t *testing.T, sandboxes []v1alpha1.Sandbox) {
				assert.Equal(t, 1, len(sandboxes))
				// available left
				assert.True(t, strings.HasPrefix(sandboxes[0].Name, "available"))
			},
		},
		{
			name:     "scale down skips locked sandboxes",
			replicas: 0,
			request: createSandboxRequest{
				createAvailableSandboxes: 1,
				lockedOwner:              "agent-user",
			},
			checkFunc: func(t *testing.T, sandboxes []v1alpha1.Sandbox) {
				assert.Equal(t, 1, len(sandboxes))
			},
		},
		{
			name:     "scale down manager-owned locked sandboxes",
			replicas: 0,
			request: createSandboxRequest{
				createAvailableSandboxes: 1,
				lockedOwner:              consts.OwnerManager,
			},
			checkFunc: func(t *testing.T, sandboxes []v1alpha1.Sandbox) {
				assert.Equal(t, 0, len(sandboxes))
			},
			expectEvents: []string{EventSandboxScaledDown},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			k8sClient := NewClient()
			eventRecorder := record.NewFakeRecorder(10)
			reconciler := &Reconciler{
				Client:   k8sClient,
				Scheme:   testScheme,
				Recorder: eventRecorder,
				Codec:    codec,
			}
			sbs := getSandboxSet(tt.replicas)
			assert.NoError(t, k8sClient.Create(ctx, sbs))
			CreateSandboxes(t, tt.request, sbs, k8sClient)

			scaleExpectation.DeleteExpectations(GetControllerKey(sbs))
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sbs)})
			assert.NoError(t, err)

			sandboxes := &v1alpha1.SandboxList{}
			assert.NoError(t, k8sClient.List(ctx, sandboxes))
			tt.checkFunc(t, sandboxes.Items)
			CheckAllEvents(t, eventRecorder, tt.expectEvents)
		})
	}
}
