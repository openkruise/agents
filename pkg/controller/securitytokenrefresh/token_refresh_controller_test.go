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

package securitytokenrefresh

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
)

func newControllerScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

// fakeRefresher records every Refresh call and either returns the configured
// response or the configured error. It lets the reconciler tests exercise the
// timing branches without exercising the real identity provider.
type fakeRefresher struct {
	resp     *identity.TokenResponse
	err      error
	calls    int
	lastSbx  *agentsv1alpha1.Sandbox
	lastTime time.Time
	now      func() time.Time
}

func (f *fakeRefresher) Refresh(ctx context.Context, sbx *agentsv1alpha1.Sandbox) (*identity.TokenResponse, error) {
	f.calls++
	f.lastSbx = sbx.DeepCopy()
	if f.now != nil {
		f.lastTime = f.now()
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// withFlags temporarily overrides the package-level flag values and returns a
// cleanup that restores them. Tests use this to lock down jitter and the
// refresh lead time so timing assertions stay deterministic.
func withFlags(leadTime time.Duration, ratio float64, retry time.Duration) func() {
	prevW, prevR, prevE := refreshLeadTime, jitterRatio, refreshRetryAfter
	refreshLeadTime, jitterRatio, refreshRetryAfter = leadTime, ratio, retry
	return func() {
		refreshLeadTime, jitterRatio, refreshRetryAfter = prevW, prevR, prevE
	}
}

func newReconciler(t *testing.T, refresher *fakeRefresher, now time.Time, sbx *agentsv1alpha1.Sandbox) (*SecurityTokenRefreshReconciler, *record.FakeRecorder, client.Client) {
	t.Helper()
	scheme := newControllerScheme(t)
	builder := fake.NewClientBuilder().WithScheme(scheme)
	if sbx != nil {
		builder = builder.WithObjects(sbx)
	}
	c := builder.Build()
	rec := record.NewFakeRecorder(16)
	r := &SecurityTokenRefreshReconciler{
		Client:    c,
		Scheme:    scheme,
		Recorder:  rec,
		refresher: refresher,
		now:       func() time.Time { return now },
		// deterministic: 0.5 → centred jitter delta is exactly 0.
		randFloat: func() float64 { return 0.5 },
	}
	if refresher != nil {
		refresher.now = r.now
	}
	return r, rec, c
}

// drainEvents non-blockingly drains the FakeRecorder so individual sub-cases
// can assert on the final emitted event without ordering dependencies.
func drainEvents(rec *record.FakeRecorder) []string {
	var got []string
	for {
		select {
		case e := <-rec.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

func TestReconcile(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	const sandboxName = "sbx-1"
	const sandboxNs = "default"

	type tc struct {
		name string
		// status sets the JSON written into utils.AgentKeyTokenRefreshStatus.
		status string
		// claimed toggles the LabelSandboxIsClaimed=true label.
		claimed       bool
		objectMissing bool
		// notServing leaves Phase/RuntimeInitialized unset so the sandbox is treated
		// as having no serving runtime; by default cases set Phase=Running and
		// RuntimeInitialized=True so the timing branches run. The gate is
		// token-independent (never the aggregate Ready condition).
		notServing bool
		// runningNoInitCond sets Phase=Running but leaves the RuntimeInitialized
		// condition ABSENT, mirroring a freshly-claimed sandbox that never went
		// through a resume/recreate re-init cycle. Such a sandbox is serving and
		// must NOT have its refresh deferred.
		runningNoInitCond bool
		refresher         *fakeRefresher
		// expected outcomes
		expectErr        string
		expectRequeueMin time.Duration
		expectRequeueMax time.Duration
		expectCalls      int
		expectEvent      string
		// expectAnnotation is checked when the fake refresher succeeds and we
		// want to make sure the reconciler did NOT touch the annotation itself
		// (defaultRefresher / fakeRefresher own that side-effect).
		expectAnnotationUnchanged bool
	}

	expireFar := now.Add(1 * time.Hour).Format(time.RFC3339)
	expireNow := now.Add(-time.Minute).Format(time.RFC3339)
	expireSoon := now.Add(refreshLeadTimeFor(t) - time.Second).Format(time.RFC3339) // already inside the refresh lead window

	cases := []tc{
		{
			name:                      "not yet due, requeue near refreshAt",
			status:                    `{"accessTokenExpiration":"` + expireFar + `"}`,
			claimed:                   true,
			refresher:                 &fakeRefresher{},
			expectErr:                 "",
			// expire=now+1h, leadTime=30m → refreshAt=now+30m, requeue ≈ 30m.
			expectRequeueMin:          29 * time.Minute,
			expectRequeueMax:          31 * time.Minute,
			expectCalls:               0,
			expectAnnotationUnchanged: true,
		},
		{
			name:    "due now, refresher succeeds, requeue based on new expiration",
			status:  `{"accessTokenExpiration":"` + expireNow + `"}`,
			claimed: true,
			refresher: &fakeRefresher{
				resp: &identity.TokenResponse{
					AccessToken:           "new-token",
					AccessTokenExpiration: now.Add(2 * time.Hour).Format(time.RFC3339),
				},
			},
			expectErr:        "",
			expectRequeueMin: time.Hour,
			expectRequeueMax: 2*time.Hour + time.Second,
			expectCalls:      1,
			expectEvent:      "TokenRefreshed",
		},
		{
			name:    "inside safety window, triggers refresh",
			status:  `{"accessTokenExpiration":"` + expireSoon + `"}`,
			claimed: true,
			refresher: &fakeRefresher{
				resp: &identity.TokenResponse{
					AccessToken:           "new",
					AccessTokenExpiration: now.Add(1 * time.Hour).Format(time.RFC3339),
				},
			},
			expectCalls: 1,
			expectEvent: "TokenRefreshed",
		},
		{
			name:    "refresher fails, requeue with refreshRetryAfter and swallow error",
			status:  `{"accessTokenExpiration":"` + expireNow + `"}`,
			claimed: true,
			refresher: &fakeRefresher{
				err: errors.New("boom"),
			},
			// Returning a nil error keeps controller-runtime from kicking in its
			// own exponential workqueue backoff, which would otherwise override
			// the fixed refreshRetryAfter we configure here. The failure is
			// already surfaced via the TokenRefreshFailed event.
			expectErr:        "",
			expectRequeueMin: defaultRefreshRetryAfter,
			expectRequeueMax: defaultRefreshRetryAfter,
			expectCalls:      1,
			expectEvent:      "TokenRefreshFailed",
		},
		{
			name:        "malformed annotation does not retry hot",
			status:      `{not json`,
			claimed:     true,
			refresher:   &fakeRefresher{},
			expectErr:   "",
			expectCalls: 0,
			expectEvent: "TokenStatusDecodeFailed",
		},
		{
			name:        "empty status object is treated as no-op",
			status:      `{}`,
			claimed:     true,
			refresher:   &fakeRefresher{},
			expectErr:   "",
			expectCalls: 0,
		},
		{
			name:        "empty expiration string is treated as no-op (defends against degraded provider fallback)",
			status:      `{"accessTokenExpiration":""}`,
			claimed:     true,
			refresher:   &fakeRefresher{},
			expectErr:   "",
			expectCalls: 0,
		},
		{
			name:        "missing annotation is treated as no-op",
			status:      ``,
			claimed:     true,
			refresher:   &fakeRefresher{},
			expectErr:   "",
			expectCalls: 0,
		},
		{
			name:        "sandbox not claimed -> skip",
			status:      `{"accessTokenExpiration":"` + expireFar + `"}`,
			claimed:     false,
			refresher:   &fakeRefresher{},
			expectErr:   "",
			expectCalls: 0,
		},
		{
			name:          "sandbox missing -> no error",
			objectMissing: true,
			refresher:     &fakeRefresher{},
			expectErr:     "",
			expectCalls:   0,
		},
		{
			// Defends against a malformed annotation expiration: the
			// annotation decodes successfully but the timestamp itself is not
			// RFC3339. The reconciler must surface a TokenExpirationInvalid
			// event and back off using refreshRetryAfter, never call the
			// refresher, and never propagate an error (which would otherwise
			// trigger controller-runtime's exponential workqueue backoff and
			// override the fixed retry interval).
			name:             "annotation expiration unparsable -> TokenExpirationInvalid event with retry backoff",
			status:           `{"accessTokenExpiration":"not-a-date"}`,
			claimed:          true,
			refresher:        &fakeRefresher{},
			expectErr:        "",
			expectRequeueMin: defaultRefreshRetryAfter,
			expectRequeueMax: defaultRefreshRetryAfter,
			expectCalls:      0,
			expectEvent:      "TokenExpirationInvalid",
		},
		{
			// Defends against a refresher that returns a TokenResponse whose
			// AccessTokenExpiration is unparsable. The current refresh has
			// already succeeded (TokenRefreshed event must still be emitted),
			// but we cannot compute the next refreshAt; the reconciler then
			// returns Result{}, nil so that the next reconcile relies on the
			// annotation watch rather than a self-scheduled requeue.
			name:    "refresh succeeds but new expiration unparsable -> no requeue, still emits TokenRefreshed",
			status:  `{"accessTokenExpiration":"` + expireNow + `"}`,
			claimed: true,
			refresher: &fakeRefresher{
				resp: &identity.TokenResponse{
					AccessToken:           "new-token",
					AccessTokenExpiration: "not-a-date",
				},
			},
			expectErr:   "",
			expectCalls: 1,
			expectEvent: "TokenRefreshed",
		},
		{
			// Provider issued a token whose TTL (10m) is shorter than the
			// configured lead time (30m). Without the short-TTL guard the
			// next requeue would be `10m - 30m = -20m` clamped to 1s,
			// spinning the controller. With the guard the next requeue is
			// `remaining/2 = 5m`, leaving the queue idle for half the
			// token's lifetime instead.
			name:    "short TTL token avoids tight requeue loop",
			status:  `{"accessTokenExpiration":"` + expireNow + `"}`,
			claimed: true,
			refresher: &fakeRefresher{
				resp: &identity.TokenResponse{
					AccessToken:           "short-ttl",
					AccessTokenExpiration: now.Add(10 * time.Minute).Format(time.RFC3339),
				},
			},
			expectErr:        "",
			expectRequeueMin: 5 * time.Minute,
			expectRequeueMax: 5*time.Minute + time.Second,
			expectCalls:      1,
			expectEvent:      "TokenRefreshed",
		},
		{
			// A sandbox with no serving runtime (e.g. paused/resuming) has no pod
			// to receive the token, so the reconciler must defer the refresh
			// entirely: no refresher call, no requeue and no event, even though the
			// token is already inside the lead window. The refresh is picked up
			// again on the no-runtime->serving predicate transition (and the resume
			// flow's annotation rewrite).
			name:                      "sandbox with no serving runtime defers refresh even when due",
			status:                    `{"accessTokenExpiration":"` + expireNow + `"}`,
			claimed:                   true,
			notServing:                true,
			refresher:                 &fakeRefresher{},
			expectErr:                 "",
			expectCalls:               0,
			expectAnnotationUnchanged: true,
		},
		{
			// A freshly-claimed sandbox is Running but never had the
			// RuntimeInitialized condition written (that condition only appears
			// during resume/recreate re-init). Its runtime was initialized by the
			// claim flow and is serving, so an absent condition must NOT defer the
			// refresh; the token has to be rotated like any other serving sandbox.
			name:              "running sandbox without RuntimeInitialized condition refreshes when due",
			status:            `{"accessTokenExpiration":"` + expireNow + `"}`,
			claimed:           true,
			runningNoInitCond: true,
			refresher: &fakeRefresher{
				resp: &identity.TokenResponse{
					AccessToken:           "new-token",
					AccessTokenExpiration: now.Add(2 * time.Hour).Format(time.RFC3339),
				},
			},
			expectErr:   "",
			expectCalls: 1,
			expectEvent: "TokenRefreshed",
		},
	}

	cleanup := withFlags(defaultRefreshLeadTime, 0, defaultRefreshRetryAfter)
	defer cleanup()

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var sbx *agentsv1alpha1.Sandbox
			if !tt.objectMissing {
				sbx = newSandbox(sandboxName, tt.claimed, tt.status, false)
				sbx.Namespace = sandboxNs
				if !tt.notServing {
					sbx.Status.Phase = agentsv1alpha1.SandboxRunning
					if !tt.runningNoInitCond {
						sbx.Status.Conditions = []metav1.Condition{{
							Type:   string(agentsv1alpha1.RuntimeInitialized),
							Status: metav1.ConditionTrue,
						}}
					}
				}
			}
			r, rec, c := newReconciler(t, tt.refresher, now, sbx)

			res, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: sandboxName, Namespace: sandboxNs},
			})

			if tt.expectErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectErr)
			}

			assert.Equal(t, tt.expectCalls, tt.refresher.calls, "refresher call count")

			if tt.expectRequeueMin > 0 || tt.expectRequeueMax > 0 {
				assert.GreaterOrEqual(t, res.RequeueAfter, tt.expectRequeueMin, "requeue lower bound")
				assert.LessOrEqual(t, res.RequeueAfter, tt.expectRequeueMax, "requeue upper bound")
			}

			events := drainEvents(rec)
			if tt.expectEvent != "" {
				found := false
				for _, e := range events {
					if assert.NotEmpty(t, e) && strings.Contains(e, tt.expectEvent) {
						found = true
						break
					}
				}
				assert.True(t, found, "event %q not found in %v", tt.expectEvent, events)
			} else {
				assert.Empty(t, events)
			}

			if tt.expectAnnotationUnchanged && sbx != nil {
				got := &agentsv1alpha1.Sandbox{}
				require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: sandboxName, Namespace: sandboxNs}, got))
				assert.Equal(t, sbx.Annotations[identity.AgentKeyTokenRefreshStatus], got.Annotations[identity.AgentKeyTokenRefreshStatus])
			}
		})
	}
}

// refreshLeadTimeFor returns the package-level refreshLeadTime at the moment
// of the call, isolating the test from import-time flag mutations done by
// other tests.
func refreshLeadTimeFor(t *testing.T) time.Duration {
	t.Helper()
	return refreshLeadTime
}

func TestParseExpiration(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		expectErr   string
		expectEqual time.Time
	}{
		{name: "empty", in: "", expectErr: "empty"},
		{name: "garbage", in: "not-a-date", expectErr: "cannot parse"},
		{
			name:        "valid rfc3339",
			in:          "2026-01-01T12:00:00Z",
			expectEqual: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseExpiration(tt.in)
			if tt.expectErr == "" {
				require.NoError(t, err)
				assert.True(t, got.Equal(tt.expectEqual))
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectErr)
			}
		})
	}
}

func TestClampRequeue(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{name: "negative", in: -time.Minute, want: minRequeueAfter},
		{name: "zero", in: 0, want: minRequeueAfter},
		{name: "below floor", in: 500 * time.Millisecond, want: minRequeueAfter},
		{name: "at floor", in: minRequeueAfter, want: minRequeueAfter},
		{name: "above floor", in: 5 * time.Minute, want: 5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, clampRequeue(tt.in))
		})
	}
}

func TestJitteredRefreshLeadTime(t *testing.T) {
	cleanup := withFlags(10*time.Minute, 0.1, defaultRefreshRetryAfter)
	defer cleanup()

	r := &SecurityTokenRefreshReconciler{}

	tests := []struct {
		name    string
		ratio   float64
		rand    func() float64
		want    time.Duration
		wantMin time.Duration
		wantMax time.Duration
	}{
		{
			name: "ratio == 0 returns raw lead time",
			rand: func() float64 { return 0.5 },
			ratio: 0,
			want: 10 * time.Minute,
		},
		{
			name:  "rand == 0.5 cancels delta",
			rand:  func() float64 { return 0.5 },
			ratio: 0.1,
			want:  10 * time.Minute,
		},
		{
			name:    "rand == 0 produces lower bound",
			rand:    func() float64 { return 0 },
			ratio:   0.1,
			wantMin: 9 * time.Minute,
			wantMax: 9*time.Minute + time.Second,
		},
		{
			name:    "ratio clamped to 0.99 with rand near 1",
			rand:    func() float64 { return 0.999999 },
			ratio:   5,
			wantMin: 19 * time.Minute,
			wantMax: 20 * time.Minute,
		},
		{
			// Defensive branch: when the lead time is so small that the
			// jittered duration truncates to <= 0 (e.g. 1ns * 0.01 = 0ns
			// after time.Duration float-to-int truncation), the function
			// must fall back to the raw refreshLeadTime instead of returning
			// a zero/negative value that would cause an immediate requeue.
			name:    "jittered truncates to 0 falls back to raw lead time",
			rand:    func() float64 { return 0 },
			ratio:   5, // clamped to 0.99 → delta = -0.99
			want:    1 * time.Nanosecond,
			wantMin: 0,
			wantMax: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jitterRatio = tt.ratio
			r.randFloat = tt.rand
			// The "jittered truncates to 0" case requires a tiny lead time so
			// the float-to-Duration truncation hits the defensive branch.
			// Other cases keep the 10m lead time established by withFlags.
			if tt.name == "jittered truncates to 0 falls back to raw lead time" {
				prev := refreshLeadTime
				refreshLeadTime = 1 * time.Nanosecond
				defer func() { refreshLeadTime = prev }()
			}
			got := r.jitteredRefreshLeadTime()
			if tt.want > 0 {
				assert.Equal(t, tt.want, got)
			} else {
				assert.GreaterOrEqual(t, got, tt.wantMin)
				assert.LessOrEqual(t, got, tt.wantMax)
			}
		})
	}
}

// TestDecodeTokenRefreshStatus locks down the contract that the reconciler
// relies on at Reconcile():
//   - empty input ALWAYS returns (nil, nil) so callers can short-circuit with
//     a single nil-check when the annotation is not yet populated; this branch
//     must NEVER surface as an error otherwise newly claimed sandboxes would
//     emit a TokenStatusDecodeFailed event on every reconcile;
//   - malformed JSON returns a wrapped error whose message starts with
//     "failed to unmarshal token refresh status" so the reconciler can keep
//     using a stable substring in its event reason / log message;
//   - well-formed JSON populates AccessTokenExpiration verbatim, including
//     timezone offsets, with unknown fields silently ignored to keep the
//     decoder forward-compatible with future status additions.
func TestDecodeTokenRefreshStatus(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		expect      *identity.TokenRefreshStatus
		expectError string
	}{
		{
			name:   "empty input yields nil status without error",
			raw:    "",
			expect: nil,
		},
		{
			name:   "empty json object yields zero-value status",
			raw:    "{}",
			expect: &identity.TokenRefreshStatus{},
		},
		{
			name:   "valid json with expiration is decoded verbatim",
			raw:    `{"accessTokenExpiration":"2099-01-01T00:00:00Z"}`,
			expect: &identity.TokenRefreshStatus{AccessTokenExpiration: "2099-01-01T00:00:00Z"},
		},
		{
			name:   "timezone offset preserved verbatim",
			raw:    `{"accessTokenExpiration":"2026-05-25T16:24:50+08:00"}`,
			expect: &identity.TokenRefreshStatus{AccessTokenExpiration: "2026-05-25T16:24:50+08:00"},
		},
		{
			name:   "unknown fields are ignored to keep the decoder forward-compatible",
			raw:    `{"accessTokenExpiration":"2099-01-01T00:00:00Z","futureField":"ignored"}`,
			expect: &identity.TokenRefreshStatus{AccessTokenExpiration: "2099-01-01T00:00:00Z"},
		},
		{
			name:        "malformed json returns wrapped error",
			raw:         "not-json",
			expectError: "failed to unmarshal token refresh status",
		},
		{
			name:        "type mismatch on expiration returns wrapped error",
			raw:         `{"accessTokenExpiration":12345}`,
			expectError: "failed to unmarshal token refresh status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeTokenRefreshStatus(tt.raw)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Nil(t, got, "decoder must not return a partial status when JSON is malformed")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expect, got)
		})
	}
}

// withSetupHooks snapshots the package-level setupHooks slice, clears it for
// the duration of the test, and returns a cleanup that restores the original
// value. Tests use this so they can drive RegisterSetupHook / runSetupHooks
// against a deterministic empty starting state without leaking registrations
// into sibling tests.
func withSetupHooks(t *testing.T) {
	t.Helper()
	prev := setupHooks
	setupHooks = nil
	t.Cleanup(func() { setupHooks = prev })
}

func TestRegisterSetupHook(t *testing.T) {
	type tc struct {
		name         string
		register     []SetupHook
		expectStored int
	}

	noop := func(manager.Manager) error { return nil }
	tests := []tc{
		{
			name:         "nil hook is dropped silently",
			register:     []SetupHook{nil},
			expectStored: 0,
		},
		{
			name:         "single hook is appended",
			register:     []SetupHook{noop},
			expectStored: 1,
		},
		{
			name:         "multiple hooks preserve registration order",
			register:     []SetupHook{noop, noop, noop},
			expectStored: 3,
		},
		{
			name:         "nil entries interleaved with real hooks are filtered",
			register:     []SetupHook{nil, noop, nil, noop},
			expectStored: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withSetupHooks(t)

			for _, h := range tt.register {
				RegisterSetupHook(h)
			}
			assert.Len(t, setupHooks, tt.expectStored)
		})
	}
}

func TestRunSetupHooks(t *testing.T) {
	type tc struct {
		name        string
		hooks       []SetupHook
		expectErr   string
		expectCalls []int // expected indices that ran, in order
	}

	tests := []tc{
		{
			name:        "no hooks registered returns nil",
			hooks:       nil,
			expectErr:   "",
			expectCalls: nil,
		},
		{
			name: "all hooks succeed and run in registration order",
			hooks: []SetupHook{
				func(manager.Manager) error { return nil },
				func(manager.Manager) error { return nil },
				func(manager.Manager) error { return nil },
			},
			expectErr:   "",
			expectCalls: []int{0, 1, 2},
		},
		{
			name: "first failing hook short-circuits and wraps the error",
			hooks: []SetupHook{
				func(manager.Manager) error { return nil },
				func(manager.Manager) error { return errors.New("boom") },
				func(manager.Manager) error { t.Fatal("must not run after a failure"); return nil },
			},
			expectErr:   "securitytokenrefresh setup hook #1 failed: boom",
			expectCalls: []int{0, 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withSetupHooks(t)

			// Re-instrument the hooks so we can record call order without
			// relying on captured state from the table literal.
			var calls []int
			for i, h := range tt.hooks {
				idx, original := i, h
				RegisterSetupHook(func(mgr manager.Manager) error {
					calls = append(calls, idx)
					return original(mgr)
				})
			}

			err := runSetupHooks(nil)

			if tt.expectErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.expectCalls, calls)
		})
	}
}

// TestReconcile_GetError covers the Get-error transparent path of Reconcile:
// any non-NotFound API failure returned by client.Get must be propagated
// verbatim so controller-runtime applies its rate-limited workqueue backoff,
// and the reconciler must not attempt a refresh nor emit any event in that
// case. NotFound is exercised via the existing "sandbox missing" sub-case in
// TestReconcile and asserted there to return a clean Result{}, nil.
func TestReconcile_GetError(t *testing.T) {
	const sandboxName = "sbx-1"
	const sandboxNs = "default"
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		injectErr   error
		expectErr   string
		expectCalls int
	}{
		{
			name:        "transient API server error is propagated",
			injectErr:   errors.New("etcd unavailable"),
			expectErr:   "etcd unavailable",
			expectCalls: 0,
		},
		{
			name: "non-NotFound status error is propagated",
			injectErr: apierrors.NewServiceUnavailable(
				"the server is currently unable to handle the request"),
			expectErr:   "unable to handle the request",
			expectCalls: 0,
		},
		{
			// NotFound must be swallowed, NOT propagated. The fake client's
			// natural NotFound is already exercised in TestReconcile via the
			// "sandbox missing" sub-case, but here we additionally confirm
			// that an interceptor-injected NotFound takes the same fast path
			// and never reaches the refresher.
			name: "NotFound from interceptor is swallowed",
			injectErr: apierrors.NewNotFound(
				schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxes"},
				sandboxName),
			expectErr:   "",
			expectCalls: 0,
		},
	}

	cleanup := withFlags(defaultRefreshLeadTime, 0, defaultRefreshRetryAfter)
	defer cleanup()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newControllerScheme(t)
			injectedErr := tt.injectErr
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
						obj client.Object, opts ...client.GetOption) error {
						return injectedErr
					},
				}).
				Build()

			refresher := &fakeRefresher{}
			rec := record.NewFakeRecorder(8)
			r := &SecurityTokenRefreshReconciler{
				Client:    c,
				Scheme:    scheme,
				Recorder:  rec,
				refresher: refresher,
				now:       func() time.Time { return now },
				randFloat: func() float64 { return 0.5 },
			}

			res, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: sandboxName, Namespace: sandboxNs},
			})

			if tt.expectErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectErr)
			}
			// On any Get failure path the reconciler must not requeue itself
			// (controller-runtime's workqueue backoff drives the next attempt
			// when err != nil; on swallowed NotFound there is nothing to do).
			assert.Equal(t, time.Duration(0), res.RequeueAfter)
			assert.False(t, res.Requeue)
			assert.Equal(t, tt.expectCalls, refresher.calls)
			assert.Empty(t, drainEvents(rec), "no event should be emitted before the sandbox is loaded")
		})
	}
}

// fakeManager is a minimal manager.Manager stub used by TestAdd to feed Add
// when the feature gate is OFF. Add must short-circuit at the gate check
// without touching the manager, so all methods return zero values and a t.Fatal
// in the unlikely path that they are invoked.
type fakeManager struct {
	manager.Manager
	t *testing.T
}

func (f *fakeManager) GetClient() client.Client {
	f.t.Fatalf("Add must not call GetClient when the feature gate is disabled")
	return nil
}
func (f *fakeManager) GetScheme() *runtime.Scheme {
	f.t.Fatalf("Add must not call GetScheme when the feature gate is disabled")
	return nil
}

// TestAdd_FeatureGateDisabled covers the early-return path of Add when the
// SecurityIdentityProvider feature gate is OFF (the default), which is the
// hot path on clusters that do not deploy an identity provider. Under that
// branch Add must return nil immediately without ever wiring the reconciler
// or running setup hooks; otherwise a misconfigured cluster would emit
// spurious controller logs and book a worker slot it does not need.
func TestAdd_FeatureGateDisabled(t *testing.T) {
	// The default feature gate state already has SecurityIdentityProvider=off.
	// We still snapshot setupHooks so that an accidental hook execution would
	// be observable as a failure rather than silently leaking state into
	// sibling tests.
	withSetupHooks(t)
	called := false
	RegisterSetupHook(func(manager.Manager) error {
		called = true
		return nil
	})

	mgr := &fakeManager{t: t}
	err := Add(mgr)
	assert.NoError(t, err, "Add must return nil when the feature gate is disabled")
	assert.False(t, called, "setup hooks must not run when the feature gate is disabled")
}

