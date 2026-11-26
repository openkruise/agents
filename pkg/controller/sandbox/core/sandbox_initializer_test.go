/*
Copyright 2025.

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

package core

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/utils"
	utilruntime "github.com/openkruise/agents/pkg/utils/runtime"
	utestutils "github.com/openkruise/agents/pkg/utils/testutils"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
	testutils "github.com/openkruise/agents/test/utils"
)

var scheme *runtime.Scheme

func init() {
	scheme = runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
}

type deferredStartupProcessStub struct {
	processconnect.UnimplementedProcessHandler
	start func(*connect.Request[process.StartRequest], *connect.ServerStream[process.StartResponse]) error
}

func (s *deferredStartupProcessStub) Start(_ context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
	return s.start(req, stream)
}

func TestInitializeDeferredStartup(t *testing.T) {
	for _, tt := range []struct {
		name             string
		mode             agentsv1alpha1.StartupMode
		claimed          bool
		skipInit         bool
		empty            bool
		fail             bool
		pending          bool
		signal           string
		current          bool
		patchRetry       bool
		lostPublication  bool
		readFailure      bool
		delayedClaim     bool
		redaction        bool
		rawError         bool
		prepareFailure   bool
		processEndError  bool
		terminalTrailer  bool
		signalQueryError bool
		change           func(*testing.T, *agentsv1alpha1.Sandbox, *corev1.Pod)
		wantError        string
	}{
		{name: "replay env and command", mode: agentsv1alpha1.StartupModeDeferred, claimed: true},
		{name: "replay skipped init", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, skipInit: true},
		{name: "empty claim still releases", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, empty: true},
		{name: "unclaimed pool never releases", mode: agentsv1alpha1.StartupModeDeferred},
		{name: "interrupted claim is not released by resume", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, pending: true},
		{name: "delayed original claim completion permits exactly one new generation replay", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, delayedClaim: true},
		{name: "lost completion response recovers the published generation", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, pending: true, signal: "new-uid"},
		{name: "stale publication never releases a new generation", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, pending: true, signal: "old-uid"},
		{name: "publication read failure fails closed", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, pending: true, fail: true},
		{name: "completed current generation never repeats postprocessing", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, current: true},
		{name: "immediate never delivers files", claimed: true},
		{name: "old generation publication read failure precedes replay", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, fail: true},
		{name: "old delivered generation with new published signal only records", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, signal: "new-uid"},
		{name: "published new generation recovers forbidden accounting next round", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, signal: "new-uid", patchRetry: true},
		{name: "replay lost publication response waits across rounds then records", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, lostPublication: true},
		{name: "signal read failure cannot erase unknown publication across rounds", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, lostPublication: true, readFailure: true},
		{name: "redaction pending forbidden signal body", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, pending: true, fail: true, redaction: true},
		{name: "redaction pending signal query transport error", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, pending: true, signalQueryError: true, redaction: true},
		{name: "redaction pending signal query error chain", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, pending: true, signalQueryError: true, redaction: true, rawError: true},
		{name: "redaction uncertain malicious transport trailer survives forbidden verification", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, lostPublication: true, readFailure: true, redaction: true},
		{name: "redaction uncertain malicious process end survives forbidden verification", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, lostPublication: true, processEndError: true, readFailure: true, redaction: true},
		{name: "redaction uncertain malicious transport trailer error chain", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, lostPublication: true, redaction: true, rawError: true},
		{name: "redaction uncertain malicious process end error chain", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, lostPublication: true, processEndError: true, redaction: true, rawError: true},
		{name: "redaction prepare malicious process end failure", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, prepareFailure: true, processEndError: true, redaction: true},
		{name: "redaction prepare malicious transport trailer failure", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, prepareFailure: true, redaction: true},
		{name: "redaction successful terminal event followed by malicious trailer", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, terminalTrailer: true, redaction: true},
		{name: "published signal cannot bypass sandbox UID fence", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, signal: "new-uid", change: func(t *testing.T, box *agentsv1alpha1.Sandbox, _ *corev1.Pod) {
			box.UID = "replaced-sandbox"
		}, wantError: "generation changed"},
		{name: "published signal cannot bypass pod UID fence", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, signal: "new-uid", change: func(_ *testing.T, _ *agentsv1alpha1.Sandbox, pod *corev1.Pod) {
			pod.UID = "replaced-pod"
		}, wantError: "generation changed"},
		{name: "published signal cannot bypass startup request fence", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, signal: "new-uid", change: func(t *testing.T, box *agentsv1alpha1.Sandbox, _ *corev1.Pod) {
			box.Annotations[agentsv1alpha1.AnnotationInitRuntimeRequest] = `{"accessToken":"access-token","command":["false"],"startupDelivered":true,"startupPodUID":"old-uid"}`
		}, wantError: "request changed"},
		{name: "published signal cannot bypass recorded token fence", mode: agentsv1alpha1.StartupModeDeferred, claimed: true, signal: "new-uid", change: func(t *testing.T, box *agentsv1alpha1.Sandbox, _ *corev1.Pod) {
			request, err := utilruntime.GetInitRuntimeRequest(box)
			require.NoError(t, err)
			require.NotNil(t, request)
			request.AccessToken = "changed-token"
			raw, err := json.Marshal(request)
			require.NoError(t, err)
			box.Annotations[agentsv1alpha1.AnnotationInitRuntimeRequest] = string(raw)
		}, wantError: "request changed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const stage = utils.SandboxEnvClaimDir + "/.startup-TEST000000"
			const dummySecret = "DUMMY-STARTUP-ERROR-SECRET"
			const dummyEnv = "DUMMY-STARTUP-ENV-SECRET"
			const dummyCommand = "DUMMY-STARTUP-COMMAND-SECRET"
			const dummyToken = "DUMMY-STARTUP-ACCESS-TOKEN"
			accessToken := utilruntime.AccessToken
			ctx := t.Context()
			var logMu sync.Mutex
			var logLines []string
			recorder := record.NewFakeRecorder(10)
			assertRedacted := func(text string) {
				for _, marker := range []string{dummySecret, dummyEnv, dummyCommand, dummyToken} {
					assert.NotContains(t, text, marker)
				}
			}
			if tt.redaction {
				accessToken = dummyToken
				logger := funcr.NewJSON(func(line string) {
					logMu.Lock()
					defer logMu.Unlock()
					logLines = append(logLines, line)
				}, funcr.Options{Verbosity: 5})
				require.True(t, logger.V(5).Enabled(), "the result and stream-close debug logs must be enabled")
				ctx = klog.NewContext(ctx, logger)
				t.Cleanup(func() {
					logMu.Lock()
					captured := strings.Join(logLines, "\n")
					logMu.Unlock()
					assertRedacted(captured)
					if tt.lostPublication || tt.prepareFailure || tt.terminalTrailer {
						assert.Contains(t, captured, `"msg":"all messages are received"`)
						assert.Contains(t, captured, `"level":5`)
					}
					var events []string
					for {
						select {
						case event := <-recorder.Events:
							assertRedacted(event)
							events = append(events, event)
						default:
							if tt.prepareFailure {
								require.Len(t, events, 1)
								assert.Contains(t, events[0], "Warning RuntimeInitialized Failed to perform initialization:")
							} else if tt.pending || tt.rawError {
								assert.Empty(t, events, "pending/uncertain outcomes must not emit failure events")
							} else {
								require.Len(t, events, 1)
								assert.Equal(t, "Normal RuntimeInitialized Initialization completed successfully", events[0])
							}
							return
						}
					}
				})
			}
			var mu sync.Mutex
			files := map[string]string{}
			var calls []string
			signal := tt.signal
			readDenied := false
			publicationAttempted := false
			processStub := &deferredStartupProcessStub{start: func(req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				mu.Lock()
				defer mu.Unlock()
				assert.Equal(t, accessToken, req.Header().Get("X-Access-Token"))
				assert.Equal(t, "Basic cm9vdDo=", req.Header().Get("Authorization"))
				cfg := req.Msg.GetProcess()
				if !assert.NotNil(t, cfg) || !assert.GreaterOrEqual(t, len(cfg.Args), 5) {
					return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid publication process"))
				}
				assert.Equal(t, "bash", cfg.Cmd)
				assert.Equal(t, "-c", cfg.Args[0])
				assert.Equal(t, "startup-delivery", cfg.Args[2])
				assert.Equal(t, utils.SandboxEnvClaimDir, cfg.Args[3])
				switch {
				case strings.Contains(cfg.Args[1], "mktemp -d"):
					calls = append(calls, "prepare")
					assert.Equal(t, []string{utils.SandboxEnvClaimDir, "new-uid"}, cfg.Args[3:])
					if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{Output: &process.ProcessEvent_DataEvent_Stdout{Stdout: []byte(stage + "\n")}}}}}); err != nil {
						return err
					}
				case strings.Contains(cfg.Args[1], "flock -n"):
					calls = append(calls, "commit")
					hasData := "1"
					if tt.empty {
						hasData = "0"
					}
					assert.Equal(t, []string{utils.SandboxEnvClaimDir, "new-uid", stage, hasData, hasData}, cfg.Args[3:])
					assert.Equal(t, "new-uid", files[stage+"/.env_ready"])
					publicationAttempted = true
					if tt.lostPublication && !tt.redaction {
						// No terminal event: publication stays unknown until the signal becomes visible.
						return connect.NewError(connect.CodeUnavailable, errors.New("publication response lost"))
					}
					if tt.lostPublication {
						if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{Output: &process.ProcessEvent_DataEvent_Stderr{Stderr: []byte(dummySecret)}}}}}); err != nil {
							return err
						}
						if tt.processEndError {
							message := "process end error: " + dummySecret
							return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{Exited: true, Error: &message}}}})
						}
						return connect.NewError(connect.CodeUnavailable, errors.New("transport trailer: "+dummySecret))
					}
					signal = "new-uid"
				case strings.Contains(cfg.Args[1], "rm -rf"):
					calls = append(calls, "cleanup")
					assert.Equal(t, []string{utils.SandboxEnvClaimDir, stage}, cfg.Args[3:])
				default:
					t.Error("unexpected startup process")
					return connect.NewError(connect.CodeInvalidArgument, errors.New("unexpected startup process"))
				}
				if tt.prepareFailure && strings.Contains(cfg.Args[1], "mktemp -d") {
					if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{Output: &process.ProcessEvent_DataEvent_Stderr{Stderr: []byte(dummySecret)}}}}}); err != nil {
						return err
					}
					if tt.processEndError {
						message := "process end error: " + dummySecret
						return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{Exited: true, ExitCode: 17, Error: &message}}}})
					}
					return connect.NewError(connect.CodeUnavailable, errors.New("transport trailer: "+dummySecret))
				}
				if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{Exited: true}}}}); err != nil {
					return err
				}
				if tt.terminalTrailer && strings.Contains(cfg.Args[1], "flock -n") {
					return connect.NewError(connect.CodeUnavailable, errors.New("transport trailer after terminal event: "+dummySecret))
				}
				return nil
			}}
			mux := http.NewServeMux()
			processPath, processHandler := processconnect.NewProcessHandler(processStub)
			mux.Handle(processPath, processHandler)
			mux.HandleFunc("/init", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				calls = append(calls, r.Method+" /init")
				assert.Equal(t, http.MethodPost, r.Method)
				var initRequest config.InitRuntimeOptions
				if err := json.NewDecoder(r.Body).Decode(&initRequest); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assert.Equal(t, accessToken, initRequest.AccessToken)
				w.WriteHeader(http.StatusNoContent)
			})
			mux.HandleFunc("/files", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				calls = append(calls, r.Method+" /files")
				assert.Equal(t, accessToken, r.Header.Get("X-Access-Token"))
				assert.Equal(t, "Basic cm9vdDo=", r.Header.Get("Authorization"))
				assert.Equal(t, "root", r.URL.Query().Get("username"))
				if tt.fail {
					message := "publication forbidden"
					if tt.redaction {
						message += ": " + dummySecret
					}
					http.Error(w, message, http.StatusForbidden)
					return
				}
				if r.Method == http.MethodGet {
					assert.Equal(t, utils.SandboxEnvClaimDir+"/.env_ready", r.URL.Query().Get("path"))
					if tt.signalQueryError {
						// An unsupported scheme fails locally; no external test endpoint is contacted.
						http.Redirect(w, r, "dummy://invalid.test/files?access_token="+dummySecret, http.StatusFound)
						return
					}
					if readDenied {
						http.Error(w, "publication forbidden: "+dummySecret, http.StatusForbidden)
						return
					}
					if signal == "" {
						http.NotFound(w, r)
						return
					}
					_, err := io.WriteString(w, signal)
					assert.NoError(t, err)
					return
				}
				assert.Equal(t, http.MethodPost, r.Method)
				file, _, err := r.FormFile("file")
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				defer file.Close()
				data, err := io.ReadAll(file)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				files[r.URL.Query().Get("path")] = string(data)
				w.WriteHeader(http.StatusNoContent)
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			request := config.InitRuntimeOptions{AccessToken: accessToken, SkipInit: tt.skipInit, StartupDelivered: tt.claimed && !tt.pending && !tt.delayedClaim}
			if request.StartupDelivered || tt.delayedClaim {
				request.StartupPodUID = "old-uid"
			}
			if tt.current {
				request.StartupPodUID = "new-uid"
			}
			if !tt.empty {
				request.EnvVars = map[string]string{"VALUE": "claim"}
				request.Command = []string{"printf", "%s", "claim-command"}
				if tt.redaction {
					request.EnvVars["SECRET"] = dummyEnv
					request.Command = append(request.Command, dummyCommand)
				}
			}
			raw, err := json.Marshal(request)
			require.NoError(t, err)
			box := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
				Name: "replay", Namespace: "default", UID: "sandbox-uid",
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationInitRuntimeRequest: string(raw), agentsv1alpha1.AnnotationRuntimeURL: server.URL,
					agentsv1alpha1.AnnotationRuntimeAccessToken: accessToken,
				}, Labels: map[string]string{},
			}}
			box.Spec.StartupMode = tt.mode
			poisonPostprocessing := func(box *agentsv1alpha1.Sandbox) {
				// Any accidental token issuance or CSI replay must fail, not silently
				// pass a recovery test with disabled post-processing fixtures.
				box.Annotations[identity.AnnotationAgentName] = "original-claim"
				box.Annotations[models.ExtensionKeyClaimWithCSIMount_MountConfig] = "invalid-csi-request"
			}
			if tt.pending || tt.current || tt.signal == "new-uid" {
				poisonPostprocessing(box)
			}
			box.Status.PodInfo.PodUID = "old-uid"
			if tt.claimed {
				box.Labels[agentsv1alpha1.LabelSandboxIsClaimed] = agentsv1alpha1.True
			}
			status := box.Status.DeepCopy()
			status.PodInfo.PodUID = "new-uid"
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: box.Name, Namespace: box.Namespace, UID: status.PodInfo.PodUID,
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(box, agentsv1alpha1.GroupVersion.WithKind("Sandbox"))},
			}}
			storedBox := box.DeepCopy()
			if tt.change != nil {
				tt.change(t, storedBox, pod)
			}
			base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(box).WithObjects(storedBox, pod).Build()
			var apiCalls []string
			forbidPatch := tt.patchRetry
			cli := interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, isPod := obj.(*corev1.Pod); isPod {
						apiCalls = append(apiCalls, "get pod")
					} else {
						apiCalls = append(apiCalls, "get sandbox")
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					apiCalls = append(apiCalls, "patch sandbox")
					if forbidPatch {
						return apierrors.NewForbidden(schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxes"}, obj.GetName(), errors.New("accounting denied"))
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			initializer := &defaultSandboxInitializer{client: cli, apiReader: cli, recorder: recorder}
			run := func() error {
				var err error
				if tt.rawError {
					err = initializer.doInitialize(ctx, box, status)
				} else if tt.redaction || tt.patchRetry || tt.lostPublication || tt.delayedClaim {
					err = initializer.Initialize(ctx, box, status)
				} else {
					err = Initialize(ctx, box, status, cli, cli, nil)
				}
				if tt.redaction {
					if err != nil {
						assertRedacted(err.Error())
					}
					if !tt.rawError {
						condition := utils.GetSandboxCondition(status, string(agentsv1alpha1.RuntimeInitialized))
						require.NotNil(t, condition)
						assertRedacted(condition.Message)
					}
				}
				return err
			}
			assertCalls := func(want []string) {
				mu.Lock()
				defer mu.Unlock()
				assert.Equal(t, want, calls, "exact calls must exclude repeated init, token, CSI, uploads and processes")
			}
			assertCondition := func(reason string, want metav1.ConditionStatus) {
				condition := utils.GetSandboxCondition(status, string(agentsv1alpha1.RuntimeInitialized))
				require.NotNil(t, condition)
				assert.Equal(t, want, condition.Status)
				assert.Equal(t, reason, condition.Reason)
			}
			assertUnchanged := func() *agentsv1alpha1.Sandbox {
				persisted := &agentsv1alpha1.Sandbox{}
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(box), persisted))
				assert.Equal(t, string(raw), persisted.Annotations[agentsv1alpha1.AnnotationInitRuntimeRequest], "failed/unconfirmed delivery must leave the old completion record unchanged")
				assert.Equal(t, storedBox.UID, persisted.UID)
				assert.Equal(t, accessToken, persisted.Annotations[agentsv1alpha1.AnnotationRuntimeAccessToken])
				return persisted
			}
			nextRound := func() {
				persisted := assertUnchanged()
				if tt.lostPublication {
					poisonPostprocessing(persisted)
					require.NoError(t, base.Update(t.Context(), persisted))
				}
				persisted.Status = *status.DeepCopy()
				require.NoError(t, base.Status().Update(t.Context(), persisted))
				box = &agentsv1alpha1.Sandbox{}
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(persisted), box))
				status = box.Status.DeepCopy()
			}
			err = run()
			if tt.rawError || tt.prepareFailure {
				require.Error(t, err)
				var containsRawCause func(error) bool
				containsRawCause = func(cause error) bool {
					if cause == nil {
						return false
					}
					if strings.Contains(cause.Error(), dummySecret) {
						return true
					}
					switch wrapped := cause.(type) {
					case interface{ Unwrap() error }:
						return containsRawCause(wrapped.Unwrap())
					case interface{ Unwrap() []error }:
						for _, child := range wrapped.Unwrap() {
							if containsRawCause(child) {
								return true
							}
						}
					}
					return false
				}
				assert.True(t, containsRawCause(err), "redaction must not destroy the original error chain")
				if tt.signalQueryError {
					require.ErrorIs(t, err, utilruntime.ErrStartupDeliveryPending)
					var urlErr *url.Error
					require.ErrorAs(t, err, &urlErr)
					assert.Contains(t, urlErr.URL, dummySecret, "the untrusted query must reach the wrapped transport error")
					assertCalls([]string{"GET /files"})
				} else {
					if !tt.processEndError {
						var rpcErr *connect.Error
						require.ErrorAs(t, err, &rpcErr)
						assert.Equal(t, connect.CodeUnavailable, rpcErr.Code())
					}
					if tt.prepareFailure {
						assertCondition(agentsv1alpha1.SandboxConditionRuntimeInitReasonFailed, metav1.ConditionFalse)
						assert.Contains(t, err.Error(), "prepare startup delivery: exited=")
						if tt.processEndError {
							assert.Contains(t, err.Error(), "exited=true exitCode=17")
						} else {
							assert.Contains(t, err.Error(), "unavailable")
						}
						assertCalls([]string{"GET /files", "POST /init", "prepare", "cleanup"})
					} else {
						require.ErrorIs(t, err, utilruntime.ErrStartupPublicationUncertain)
						assert.Contains(t, err.Error(), "commit startup delivery: exited=")
						assertCalls([]string{"GET /files", "POST /init", "prepare", "POST /files", "POST /files", "POST /files", "commit", "GET /files"})
					}
				}
				assert.Empty(t, apiCalls)
				assertUnchanged()
				return
			}
			if tt.redaction && tt.pending {
				require.NoError(t, err, "the real wrapper must treat an incomplete claim as pending, not failed")
				assertCondition("StartupDeliveryPending", metav1.ConditionFalse)
				condition := utils.GetSandboxCondition(status, string(agentsv1alpha1.RuntimeInitialized))
				message := utilruntime.ErrStartupDeliveryPending.Error()
				if tt.fail {
					message += " (HTTP 403)"
				}
				assert.Equal(t, message, condition.Message)
				assertCalls([]string{"GET /files"})
				assert.Empty(t, apiCalls)
				nextRound()
				require.NoError(t, run())
				assertCondition("StartupDeliveryPending", metav1.ConditionFalse)
				assert.Equal(t, message, utils.GetSandboxCondition(status, string(agentsv1alpha1.RuntimeInitialized)).Message)
				assertCalls([]string{"GET /files", "GET /files"})
				assert.Empty(t, apiCalls, "pending recovery must only verify publication")
				assertUnchanged()
				return
			}
			if tt.delayedClaim {
				require.NoError(t, err)
				assertCondition("StartupDeliveryPending", metav1.ConditionFalse)
				assertCalls([]string{"GET /files"})
				assert.Empty(t, apiCalls, "the original claim still owns post-processing and accounting")
				mu.Lock()
				assert.Empty(t, files)
				assert.Empty(t, signal, "the new Pod signal must return 404")
				mu.Unlock()
				nextRound()

				completedClaim := request
				completedClaim.StartupDelivered = true
				completedRaw, marshalErr := json.Marshal(completedClaim)
				require.NoError(t, marshalErr)
				box.Annotations[agentsv1alpha1.AnnotationInitRuntimeRequest] = string(completedRaw)
				require.NoError(t, base.Update(t.Context(), box))
				box = &agentsv1alpha1.Sandbox{}
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(storedBox), box))
				status = box.Status.DeepCopy()
				assertCondition("StartupDeliveryPending", metav1.ConditionFalse)
				assert.Equal(t, string(completedRaw), box.Annotations[agentsv1alpha1.AnnotationInitRuntimeRequest])
				completed, decodeErr := utilruntime.GetInitRuntimeRequest(box)
				require.NoError(t, decodeErr)
				completedClaim.ReInit = true
				require.Equal(t, &completedClaim, completed, "external completion changes only startupDelivered, retaining the old Pod UID and payload")
				assert.Equal(t, "old-uid", completed.StartupPodUID)
				assert.Equal(t, types.UID("new-uid"), status.PodInfo.PodUID)

				err = run()
				require.NoError(t, err)
				assertCondition(agentsv1alpha1.SandboxConditionRuntimeInitReasonSucceeded, metav1.ConditionTrue)
				wantCalls := []string{"GET /files", "GET /files", "POST /init", "prepare", "POST /files", "POST /files", "POST /files", "commit", "cleanup"}
				assertCalls(wantCalls)
				wantAPICalls := []string{"get sandbox", "get pod", "patch sandbox"}
				assert.Equal(t, wantAPICalls, apiCalls)

				persisted := &agentsv1alpha1.Sandbox{}
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(box), persisted))
				recorded, decodeErr := utilruntime.GetInitRuntimeRequest(persisted)
				require.NoError(t, decodeErr)
				completedClaim.StartupPodUID = "new-uid"
				require.Equal(t, &completedClaim, recorded, "replay records the new generation without changing payload or token")
				assert.Equal(t, storedBox.UID, persisted.UID)
				assert.Equal(t, utilruntime.AccessToken, persisted.Annotations[agentsv1alpha1.AnnotationRuntimeAccessToken])
				persisted.Status = *status.DeepCopy()
				require.NoError(t, base.Status().Update(t.Context(), persisted))
				box = &agentsv1alpha1.Sandbox{}
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(persisted), box))
				status = box.Status.DeepCopy()
				err = run()
				require.NoError(t, err)
				assertCondition(agentsv1alpha1.SandboxConditionRuntimeInitReasonSucceeded, metav1.ConditionTrue)
				assertCalls(wantCalls)
				assert.Equal(t, wantAPICalls, apiCalls, "a third round must not replay or record a second time")
			}
			if tt.patchRetry {
				require.True(t, apierrors.IsForbidden(err), "the first round must propagate the real failed accounting patch")
				assertCondition(agentsv1alpha1.SandboxConditionRuntimeInitReasonFailed, metav1.ConditionFalse)
				assertCalls([]string{"GET /files"})
				assert.Equal(t, []string{"get sandbox", "get pod", "patch sandbox"}, apiCalls, "Forbidden is terminal within the round")
				nextRound()
				forbidPatch = false
				err = run()
				require.NoError(t, err)
				assertCalls([]string{"GET /files", "GET /files"})
				assert.Equal(t, []string{"get sandbox", "get pod", "patch sandbox", "get sandbox", "get pod", "patch sandbox"}, apiCalls)
				assertCondition(agentsv1alpha1.SandboxConditionRuntimeInitReasonSucceeded, metav1.ConditionTrue)
			}
			if tt.lostPublication {
				require.NoError(t, err, "wrapper must preserve uncertain publication without returning an error")
				assertCondition(startupPublicationUncertainReason, metav1.ConditionFalse)
				condition := utils.GetSandboxCondition(status, string(agentsv1alpha1.RuntimeInitialized))
				assert.Equal(t, utilruntime.ErrStartupPublicationUncertain.Error(), condition.Message)
				firstRound := []string{"GET /files", "POST /init", "prepare", "POST /files", "POST /files", "POST /files", "commit", "GET /files"}
				assertCalls(firstRound)
				assert.Empty(t, apiCalls, "unconfirmed publication must not account delivery")
				nextRound()
				expectedCalls := append([]string{}, firstRound...)
				if tt.readFailure {
					mu.Lock()
					readDenied = true
					mu.Unlock()
					if tt.redaction {
						verifyErr := initializer.doInitialize(ctx, box, status)
						require.ErrorIs(t, verifyErr, utilruntime.ErrStartupPublicationUncertain)
						var apiErr *utilruntime.APIError
						require.ErrorAs(t, verifyErr, &apiErr)
						assert.Equal(t, http.StatusForbidden, apiErr.StatusCode)
						assertRedacted(verifyErr.Error())
						expectedCalls = append(expectedCalls, "GET /files")
						assertCalls(expectedCalls)
						assertUnchanged()
					}
					require.NoError(t, run())
					assertCondition(startupPublicationUncertainReason, metav1.ConditionFalse)
					condition := utils.GetSandboxCondition(status, string(agentsv1alpha1.RuntimeInitialized))
					assert.Equal(t, utilruntime.ErrStartupPublicationUncertain.Error()+" (HTTP 403)", condition.Message)
					assert.Empty(t, recorder.Events, "an uncertain publication read failure must not emit a warning")
					expectedCalls = append(expectedCalls, "GET /files")
					assertCalls(expectedCalls)
					assert.Empty(t, apiCalls)
					nextRound()
					mu.Lock()
					readDenied = false
					mu.Unlock()
				}
				require.NoError(t, run())
				assertCondition(startupPublicationUncertainReason, metav1.ConditionFalse)
				assert.Equal(t, utilruntime.ErrStartupPublicationUncertain.Error(), utils.GetSandboxCondition(status, string(agentsv1alpha1.RuntimeInitialized)).Message)
				assert.Empty(t, recorder.Events, "unknown publication remains a wait, not a failed initialization")
				expectedCalls = append(expectedCalls, "GET /files")
				assertCalls(expectedCalls)
				assert.Empty(t, apiCalls, "uncertain replay may only verify publication")
				nextRound()
				mu.Lock()
				require.True(t, publicationAttempted)
				signal = "new-uid"
				mu.Unlock()
				err = run()
				require.NoError(t, err)
				assertCondition(agentsv1alpha1.SandboxConditionRuntimeInitReasonSucceeded, metav1.ConditionTrue)
				expectedCalls = append(expectedCalls, "GET /files")
				assertCalls(expectedCalls)
				assert.Equal(t, []string{"get sandbox", "get pod", "patch sandbox"}, apiCalls, "visible publication must only record completion")
			}
			mu.Lock()
			defer mu.Unlock()
			if tt.current {
				require.NoError(t, err)
				assert.Empty(t, calls)
				assert.Empty(t, files)
				assert.Empty(t, apiCalls)
				return
			}
			if tt.pending && tt.signal != "new-uid" && !tt.fail {
				require.ErrorIs(t, err, utilruntime.ErrStartupDeliveryPending)
				assert.Equal(t, []string{"GET /files"}, calls)
				assert.Empty(t, files)
				assert.Empty(t, apiCalls)
				return
			}
			if tt.fail || tt.wantError != "" {
				require.Error(t, err)
				assert.Equal(t, []string{"GET /files"}, calls)
				assert.Empty(t, files)
				if tt.fail {
					var apiError *utilruntime.APIError
					require.ErrorAs(t, err, &apiError)
					assert.Equal(t, http.StatusForbidden, apiError.StatusCode)
					assert.Empty(t, apiCalls)
				} else {
					assert.Contains(t, err.Error(), tt.wantError)
					assert.Equal(t, []string{"get sandbox", "get pod"}, apiCalls, "generation/request/token fences must reject accounting before Patch")
				}
				return
			}
			require.NoError(t, err)
			if !tt.patchRetry && !tt.lostPublication && !tt.delayedClaim {
				assert.Equal(t, types.UID("old-uid"), box.Status.PodInfo.PodUID)
			}
			if !tt.claimed || tt.mode != agentsv1alpha1.StartupModeDeferred {
				assert.Equal(t, []string{"POST /init"}, calls)
				assert.Empty(t, files)
				assert.Empty(t, apiCalls)
				return
			}
			if tt.signal == "new-uid" {
				assert.Empty(t, files, "published recovery must not replay startup files")
				if !tt.patchRetry {
					assert.Equal(t, []string{"GET /files"}, calls)
					assert.Equal(t, []string{"get sandbox", "get pod", "patch sandbox"}, apiCalls)
				}
			} else {
				assert.Equal(t, "new-uid", files[stage+"/.env_ready"])
				if !tt.lostPublication {
					wantCalls := []string{"GET /files"}
					if tt.delayedClaim {
						wantCalls = append(wantCalls, "GET /files")
					}
					if !tt.skipInit {
						wantCalls = append(wantCalls, "POST /init")
					}
					wantCalls = append(wantCalls, "prepare")
					if !tt.empty {
						wantCalls = append(wantCalls, "POST /files", "POST /files")
					}
					wantCalls = append(wantCalls, "POST /files", "commit", "cleanup")
					assert.Equal(t, wantCalls, calls)
					assert.Equal(t, []string{"get sandbox", "get pod", "patch sandbox"}, apiCalls)
				}
				if tt.empty {
					assert.Len(t, files, 1)
				} else {
					assert.Len(t, files, 3)
					assert.Contains(t, files[stage+"/env.sh"], "export VALUE='claim'")
					assert.Contains(t, files[stage+"/cmd.sh"], "exec -- 'printf' '%s' 'claim-command'")
				}
			}
			persisted := &agentsv1alpha1.Sandbox{}
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(box), persisted))
			recorded, err := utilruntime.GetInitRuntimeRequest(persisted)
			require.NoError(t, err)
			require.NotNil(t, recorded)
			assert.True(t, recorded.StartupDelivered)
			assert.Equal(t, "new-uid", recorded.StartupPodUID)
			assert.Equal(t, request.AccessToken, recorded.AccessToken)
			assert.Equal(t, request.EnvVars, recorded.EnvVars)
			assert.Equal(t, request.Command, recorded.Command)
			assert.Equal(t, storedBox.UID, persisted.UID)
			assert.Equal(t, accessToken, persisted.Annotations[agentsv1alpha1.AnnotationRuntimeAccessToken])
			if tt.pending || tt.signal == "new-uid" || tt.lostPublication {
				assert.Equal(t, "original-claim", persisted.Annotations[identity.AnnotationAgentName])
				assert.Equal(t, "invalid-csi-request", persisted.Annotations[models.ExtensionKeyClaimWithCSIMount_MountConfig])
			}
		})
	}
}

func TestInitialize(t *testing.T) {
	utestutils.InitLogOutput()
	newFakeClient := func(initObj ...client.Object) client.Client {
		return fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObj...).Build()
	}
	tests := []struct {
		name            string
		box             *agentsv1alpha1.Sandbox
		newStatus       *agentsv1alpha1.SandboxStatus
		setupClients    func() (client.Client, client.Reader)
		storageRegistry storages.VolumeMountProviderRegistry
		expectError     string
		useRuntimeSvr   bool
		serverOpts      testutils.TestRuntimeServerOptions
	}{
		{
			name: "nil client returns nil",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{},
			setupClients: func() (c client.Client, reader client.Reader) {
				return nil, fake.NewFakeClient()
			},
		},
		{
			name: "nil apiReader returns nil",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{},
			setupClients: func() (c client.Client, reader client.Reader) {
				return fake.NewFakeClient(), nil
			},
		},
		{
			name: "sandbox not claimed by SandboxClaim - skip initialization",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels:    map[string]string{},
				},
			},
			newStatus:       &agentsv1alpha1.SandboxStatus{},
			useRuntimeSvr:   false,
			storageRegistry: storages.NewStorageProvider(),
		},
		{
			name: "claimed sandbox with no init runtime annotation and no csi mount annotation - success",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxClaimName: "my-claim",
					},
					Annotations: map[string]string{},
				},
			},
			newStatus:       &agentsv1alpha1.SandboxStatus{},
			useRuntimeSvr:   false,
			storageRegistry: storages.NewStorageProvider(),
		},
		{
			name: "claimed sandbox with init runtime annotation - re-init runtime success",
			box: func() *agentsv1alpha1.Sandbox {
				initOpts := config.InitRuntimeOptions{
					AccessToken: "test-token",
					EnvVars:     map[string]string{"VAR1": "val1"},
				}
				initJSON, _ := json.Marshal(initOpts)
				return &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-reinit",
						Namespace: "default",
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxClaimName: "my-claim",
						},
						Annotations: map[string]string{
							agentsv1alpha1.AnnotationInitRuntimeRequest: string(initJSON),
						},
					},
				}
			}(),
			newStatus: &agentsv1alpha1.SandboxStatus{},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: utilruntime.RunCommandResult{
					PID:    1,
					Exited: true,
				},
				RunCommandImmediately: true,
			},
			useRuntimeSvr:   true,
			storageRegistry: storages.NewStorageProvider(),
		},
		{
			name: "claimed sandbox with init runtime annotation - re-init runtime failure",
			box: func() *agentsv1alpha1.Sandbox {
				initOpts := config.InitRuntimeOptions{
					AccessToken: "test-token",
				}
				initJSON, _ := json.Marshal(initOpts)
				return &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-reinit-fail",
						Namespace: "default",
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxClaimName: "my-claim",
						},
						Annotations: map[string]string{
							agentsv1alpha1.AnnotationInitRuntimeRequest: string(initJSON),
						},
					},
				}
			}(),
			newStatus: &agentsv1alpha1.SandboxStatus{},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: utilruntime.RunCommandResult{
					PID:    1,
					Exited: true,
				},
				RunCommandImmediately: true,
				InitErrCode:           500,
			},
			useRuntimeSvr:   true,
			storageRegistry: storages.NewStorageProvider(),
			expectError:     "returned status 500",
		},
		{
			name: "claimed sandbox with invalid init runtime annotation JSON",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox-bad-init-json",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxClaimName: "my-claim",
					},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationInitRuntimeRequest: "not-valid-json",
					},
				},
			},
			newStatus:       &agentsv1alpha1.SandboxStatus{},
			useRuntimeSvr:   false,
			storageRegistry: storages.NewStorageProvider(),
			expectError:     "failed to unmarshal init runtime request",
		},
		{
			name: "claimed sandbox with invalid csi mount annotation JSON",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox-bad-csi-json",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxClaimName: "my-claim",
					},
					Annotations: map[string]string{
						models.ExtensionKeyClaimWithCSIMount_MountConfig: "not-valid-json",
					},
				},
			},
			newStatus:       &agentsv1alpha1.SandboxStatus{},
			useRuntimeSvr:   false,
			storageRegistry: storages.NewStorageProvider(),
			expectError:     "failed to get csi mount request",
		},
		{
			name: "claimed sandbox with csi mount annotation - pv not found",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox-csi-pv-missing",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxClaimName: "my-claim",
					},
					Annotations: map[string]string{
						models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"non-existent-pv","mountPath":"/data"}]`,
					},
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: utilruntime.RunCommandResult{
					PID:    1,
					Exited: true,
				},
				RunCommandImmediately: true,
			},
			useRuntimeSvr:   true,
			storageRegistry: storages.NewStorageProvider(),
			expectError:     "failed to generate csi mount options config",
		},
		{
			name: "claimed sandbox with csi mount annotation - driver supported, mount success",
			box: func() *agentsv1alpha1.Sandbox {
				return &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-csi-ok",
						Namespace: "default",
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxClaimName: "my-claim",
						},
						Annotations: map[string]string{
							models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"test-pv-ok","mountPath":"/data"}]`,
						},
					},
				}
			}(),
			newStatus: &agentsv1alpha1.SandboxStatus{},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: utilruntime.RunCommandResult{
					PID:      1,
					ExitCode: 0,
					Exited:   true,
				},
				RunCommandImmediately: true,
			},
			useRuntimeSvr: true,
			storageRegistry: func() storages.VolumeMountProviderRegistry {
				reg := storages.NewStorageProvider()
				reg.RegisterProvider("test-csi-driver", &storages.MountProvider{})
				return reg
			}(),
			setupClients: func() (client.Client, client.Reader) {
				c := newFakeClient(&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pv-ok",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver:       "test-csi-driver",
								VolumeHandle: "handle-ok",
							},
						},
					},
				})
				return c, c
			},
		},
		{
			name: "claimed sandbox with csi mount annotation - mount command failure",
			box: func() *agentsv1alpha1.Sandbox {
				return &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-csi-mount-fail",
						Namespace: "default",
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxClaimName: "my-claim",
						},
						Annotations: map[string]string{
							models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"test-pv-fail","mountPath":"/data"}]`,
						},
					},
				}
			}(),
			newStatus: &agentsv1alpha1.SandboxStatus{},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: utilruntime.RunCommandResult{
					PID:      1,
					ExitCode: 1,
					Stderr:   []string{"mount error"},
					Exited:   true,
				},
				RunCommandImmediately: true,
			},
			useRuntimeSvr: true,
			storageRegistry: func() storages.VolumeMountProviderRegistry {
				reg := storages.NewStorageProvider()
				reg.RegisterProvider("test-csi-driver-fail", &storages.MountProvider{})
				return reg
			}(),
			setupClients: func() (client.Client, client.Reader) {
				c := newFakeClient(&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pv-fail",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver:       "test-csi-driver-fail",
								VolumeHandle: "handle-fail",
							},
						},
					},
				})
				return c, c
			},
			expectError: "failed to perform ReCSIMount after resume",
		},
		{
			name: "claimed sandbox with multiple csi mount annotations - partial failure returns joined errors",
			box: func() *agentsv1alpha1.Sandbox {
				return &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-csi-multi-fail",
						Namespace: "default",
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxClaimName: "my-claim",
						},
						Annotations: map[string]string{
							models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"test-pv-multi-1","mountPath":"/data1"},{"pvName":"test-pv-multi-2","mountPath":"/data2"}]`,
						},
					},
				}
			}(),
			newStatus: &agentsv1alpha1.SandboxStatus{},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: utilruntime.RunCommandResult{
					PID:      1,
					ExitCode: 1,
					Stderr:   []string{"mount failed"},
					Exited:   true,
				},
				RunCommandImmediately: true,
			},
			useRuntimeSvr: true,
			storageRegistry: func() storages.VolumeMountProviderRegistry {
				reg := storages.NewStorageProvider()
				reg.RegisterProvider("test-multi-driver", &storages.MountProvider{})
				return reg
			}(),
			setupClients: func() (client.Client, client.Reader) {
				var pvs []client.Object
				for _, pvName := range []string{"test-pv-multi-1", "test-pv-multi-2"} {
					pv := &corev1.PersistentVolume{
						ObjectMeta: metav1.ObjectMeta{
							Name: pvName,
						},
						Spec: corev1.PersistentVolumeSpec{
							PersistentVolumeSource: corev1.PersistentVolumeSource{
								CSI: &corev1.CSIPersistentVolumeSource{
									Driver:       "test-multi-driver",
									VolumeHandle: pvName + "-handle",
								},
							},
						},
					}
					pvs = append(pvs, pv)
				}
				c := newFakeClient(pvs...)
				return c, c
			},
			expectError: "failed to perform ReCSIMount after resume",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c client.Client
			var r client.Reader
			if tt.setupClients != nil {
				c, r = tt.setupClients()
			} else {
				f := newFakeClient()
				c, r = f, f
			}
			if tt.useRuntimeSvr {
				server := testutils.NewTestRuntimeServer(tt.serverOpts)
				defer server.Close()

				if tt.box.Annotations == nil {
					tt.box.Annotations = map[string]string{}
				}
				tt.box.Annotations[agentsv1alpha1.AnnotationRuntimeURL] = server.URL
				tt.box.Annotations[agentsv1alpha1.AnnotationRuntimeAccessToken] = utilruntime.AccessToken
			}

			err := Initialize(t.Context(), tt.box, tt.newStatus, c, r, tt.storageRegistry)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInitialize_InitTransportDispatch verifies the init-handshake side of the
// two coexisting runtime transports: non-empty rtOpts route the /init call to
// the endpoint they select instead of the legacy annotation-resolved URL, so a
// TLS-capable sandbox reaches the agent-runtime sidecar over HTTPS. The options
// are the real ones TransportOptionsFor produces (WithTLS + WithTLSPort), with
// the authority pointed at the httptest certificate hostname so verification
// succeeds while the dial is pinned to the sandbox Pod IP. The legacy path
// (empty rtOpts) is covered by TestInitialize above.
func TestInitialize_InitTransportDispatch(t *testing.T) {
	utestutils.InitLogOutput()

	newBox := func(name string) *agentsv1alpha1.Sandbox {
		initOpts := config.InitRuntimeOptions{
			AccessToken: "test-token",
			EnvVars:     map[string]string{"VAR1": "val1"},
		}
		initJSON, err := json.Marshal(initOpts)
		require.NoError(t, err)
		return &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels: map[string]string{
					agentsv1alpha1.LabelSandboxClaimName: "my-claim",
				},
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationInitRuntimeRequest: string(initJSON),
				},
			},
		}
	}

	tests := []struct {
		name        string
		initStatus  int
		expectError string
	}{
		{
			name:       "init routed to rtOpts endpoint",
			initStatus: http.StatusNoContent,
		},
		{
			// GetInitRuntimeRequest always sets ReInit=true, so a 401 from the
			// dispatched endpoint is classified as "already initialized".
			name:       "init 401 treated as success on re-init",
			initStatus: http.StatusUnauthorized,
		},
		{
			name:        "init server error propagates",
			initStatus:  http.StatusInternalServerError,
			expectError: "returned status 500",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The dispatch server stands in for the agent-runtime HTTPS endpoint
			// selected by rtOpts; it must receive the /init handshake.
			var gotInit config.InitRuntimeOptions
			var dispatchHits atomic.Int32
			dispatchSvr := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "/init", r.URL.Path)
				dispatchHits.Add(1)
				require.NoError(t, json.NewDecoder(r.Body).Decode(&gotInit))
				w.WriteHeader(tt.initStatus)
			}))
			defer dispatchSvr.Close()

			// The httptest certificate is issued for "example.com" and the
			// loopback IPs; trust it and address it by that authority so the
			// handshake validates while the dial is pinned to the Pod IP.
			caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: dispatchSvr.Certificate().Raw})
			podIP, portStr, err := net.SplitHostPort(dispatchSvr.Listener.Addr().String())
			require.NoError(t, err)
			tlsPort, err := strconv.Atoi(portStr)
			require.NoError(t, err)

			// The legacy server backs the annotation-resolved runtime URL; with
			// non-empty rtOpts it must never see the /init handshake.
			var legacyInitHits atomic.Int32
			legacySvr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/init" {
					legacyInitHits.Add(1)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer legacySvr.Close()

			box := newBox("test-sandbox-init-dispatch")
			box.Annotations[agentsv1alpha1.AnnotationRuntimeURL] = legacySvr.URL
			box.Status.PodInfo.PodIP = podIP

			fc := fake.NewClientBuilder().WithScheme(scheme).Build()
			err = Initialize(t.Context(), box, &box.Status, fc, fc, storages.NewStorageProvider(),
				utilruntime.WithTLS(utilruntime.TLSBundle{CABundle: caPEM}),
				utilruntime.WithAuthority("example.com"),
				utilruntime.WithTLSPort(tlsPort),
				utilruntime.WithRetry(wait.Backoff{Duration: time.Millisecond, Steps: 1}))

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
			}
			assert.GreaterOrEqual(t, dispatchHits.Load(), int32(1), "init must reach the rtOpts-selected endpoint")
			assert.Equal(t, int32(0), legacyInitHits.Load(), "init must not fall back to the legacy annotation URL")
			assert.Equal(t, "test-token", gotInit.AccessToken)
			assert.Equal(t, map[string]string{"VAR1": "val1"}, gotInit.EnvVars)
		})
	}
}

func TestDefaultSandboxInitializer(t *testing.T) {
	utestutils.InitLogOutput()

	tests := []struct {
		name                string
		box                 *agentsv1alpha1.Sandbox
		expectError         string
		expectInitCondition metav1.ConditionStatus
		expectInitReason    string
	}{
		{
			name: "unclaimed sandbox - skip initialization, set RuntimeInitialized=True",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox-default-init",
					Namespace: "default",
					Labels:    map[string]string{},
				},
			},
			expectInitCondition: metav1.ConditionTrue,
			expectInitReason:    agentsv1alpha1.SandboxConditionRuntimeInitReasonSucceeded,
		},
		{
			name: "claimed sandbox with invalid init runtime annotation - error path sets RuntimeInitialized=False",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox-init-fail",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxClaimName: "my-claim",
					},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationInitRuntimeRequest: "not-valid-json",
					},
				},
			},
			expectError:         "failed to unmarshal init runtime request",
			expectInitCondition: metav1.ConditionFalse,
			expectInitReason:    agentsv1alpha1.SandboxConditionRuntimeInitReasonFailed,
		},
		{
			// The initializer holds no TLS bundle here, mirroring a controller
			// started without --runtime-client-cert-dir. The transport is now
			// resolved for every initialization, so a sandbox stamped by an
			// earlier configuration fails loudly instead of falling back to the
			// plaintext /init and mount paths.
			name: "tls-capable sandbox without tls bundle - RuntimeInitialized=False",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox-tls-no-bundle",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxClaimName: "my-claim",
					},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationRuntimeTLSPort: "49984",
					},
				},
			},
			expectError:         "advertises runtime TLS port",
			expectInitCondition: metav1.ConditionFalse,
			expectInitReason:    agentsv1alpha1.SandboxConditionRuntimeInitReasonFailed,
		},
		{
			// A broken capability annotation points at a broken injection
			// template, so it must surface rather than silently select the
			// plaintext transport.
			name: "invalid tls port annotation - RuntimeInitialized=False",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox-bad-tls-port",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxClaimName: "my-claim",
					},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationRuntimeTLSPort: "not-a-port",
					},
				},
			},
			expectError:         "invalid runtime TLS port annotation",
			expectInitCondition: metav1.ConditionFalse,
			expectInitReason:    agentsv1alpha1.SandboxConditionRuntimeInitReasonFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := fake.NewClientBuilder().WithScheme(scheme).Build()
			recorder := record.NewFakeRecorder(10)

			initializer := &defaultSandboxInitializer{
				client:          fc,
				apiReader:       fc,
				storageRegistry: storages.NewStorageProvider(),
				recorder:        recorder,
			}

			newStatus := &agentsv1alpha1.SandboxStatus{}
			err := initializer.Initialize(t.Context(), tt.box, newStatus)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}

			initCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.RuntimeInitialized))
			require.NotNil(t, initCond, "expected RuntimeInitialized condition to be set")
			assert.Equal(t, tt.expectInitCondition, initCond.Status)
			assert.Equal(t, tt.expectInitReason, initCond.Reason)
		})
	}
}
