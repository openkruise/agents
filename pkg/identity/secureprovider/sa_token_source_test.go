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

package secureprovider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/openkruise/agents/pkg/utils"
)

// fakeSATokenClient returns a fake clientset whose ServiceAccount token subresource
// answers through respond, plus an accessor for the TokenRequests received so far. A real
// API server is not available in unit tests and the subresource is the trust boundary
// under test, so the fake is the narrowest usable seam.
//
// respond receives the zero-based call index and the namespace the request was issued in.
func fakeSATokenClient(respond func(call int, namespace string, req *authenticationv1.TokenRequest) (string, time.Time, error)) (*kubernetesfake.Clientset, func() []*authenticationv1.TokenRequest) {
	clientset := kubernetesfake.NewSimpleClientset()

	var mu sync.Mutex
	var requests []*authenticationv1.TokenRequest

	clientset.PrependReactor("create", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create := action.(k8stesting.CreateAction)
		if create.GetSubresource() != "token" {
			return false, nil, nil
		}
		req := create.GetObject().(*authenticationv1.TokenRequest)

		mu.Lock()
		requests = append(requests, req)
		call := len(requests) - 1
		mu.Unlock()

		token, expiration, err := respond(call, create.GetNamespace(), req)
		if err != nil {
			return true, nil, err
		}
		out := &authenticationv1.TokenRequest{
			Status: authenticationv1.TokenRequestStatus{Token: token},
		}
		if !expiration.IsZero() {
			out.Status.ExpirationTimestamp = metav1.NewTime(expiration)
		}
		return true, out, nil
	})

	return clientset, func() []*authenticationv1.TokenRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]*authenticationv1.TokenRequest(nil), requests...)
	}
}

// ---------------------------------------------------------------------------
// Refresh timing: pure functions
// ---------------------------------------------------------------------------

func TestNextRefreshDelay(t *testing.T) {
	tests := []struct {
		name      string
		available time.Duration
		want      time.Duration
	}{
		{
			// The design's observable example: an hour was requested but half an hour
			// was granted, so the schedule follows the granted lifetime.
			name:      "granted half hour refreshes after 24 minutes",
			available: 30 * time.Minute,
			want:      24 * time.Minute,
		},
		{
			name:      "short granted lifetime is floored",
			available: 1200 * time.Millisecond,
			want:      minRefreshDelay,
		},
		{
			name:      "floor never waits past a short granted lifetime",
			available: 300 * time.Millisecond,
			want:      300 * time.Millisecond,
		},
		{
			name:      "no remaining lifetime refreshes immediately",
			available: 0,
			want:      0,
		},
		{
			name:      "expired lifetime refreshes immediately",
			available: -time.Minute,
			want:      0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nextRefreshDelay(tt.available)
			assert.Equal(t, tt.want, got)
			if tt.available > 0 {
				assert.LessOrEqual(t, got, tt.available, "a refresh must never be scheduled past expiration")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// token: snapshot semantics
// ---------------------------------------------------------------------------

func TestSATokenSource_Token(t *testing.T) {
	tests := []struct {
		name       string
		token      string
		expiration time.Time
		want       string
		wantErr    string
	}{
		{
			name:    "unissued credential is reported",
			wantErr: "has not been issued",
		},
		{
			name:       "valid credential is returned",
			token:      "tok-1",
			expiration: time.Now().Add(time.Minute),
			want:       "tok-1",
		},
		{
			name:       "credential at its expiration is not returned",
			token:      "tok-1",
			expiration: time.Now(),
			wantErr:    "expired at",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &saTokenSource{cachedToken: tt.token, cachedExpiration: tt.expiration}

			got, err := s.token()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Empty(t, got, "an unusable credential must not reach a caller")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// fetch: request shape and response validation
// ---------------------------------------------------------------------------

func TestSATokenSource_Fetch(t *testing.T) {
	const saName = "sandbox-manager"
	validExpiration := time.Now().Add(time.Hour)

	tests := []struct {
		name       string
		token      string
		expiration time.Time
		apiErr     error
		wantErr    string
	}{
		{
			name:       "valid response is returned",
			token:      "tok-1",
			expiration: validExpiration,
		},
		{
			name:       "empty token is rejected",
			expiration: validExpiration,
			wantErr:    "returned an empty token",
		},
		{
			name:    "missing expiration is rejected",
			token:   "tok-1",
			wantErr: "returned no expiration",
		},
		{
			name:       "already expired token is rejected",
			token:      "tok-1",
			expiration: time.Now().Add(-time.Minute),
			wantErr:    "returned an already expired token",
		},
		{
			name:    "API failure is wrapped with the resource identity",
			apiErr:  errors.New("forbidden"),
			wantErr: "failed to request identity provider service account token for " + utils.DefaultSandboxDeployNamespace + "/" + saName,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset, _ := fakeSATokenClient(func(int, string, *authenticationv1.TokenRequest) (string, time.Time, error) {
				return tt.token, tt.expiration, tt.apiErr
			})
			s := &saTokenSource{clientset: clientset, saName: saName}

			token, expiration, err := s.fetch(context.Background())
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Empty(t, token)
				assert.True(t, expiration.IsZero())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.token, token)
			assert.Equal(t, validExpiration.Unix(), expiration.Unix())
		})
	}
}

func TestSATokenSource_Fetch_RequestSpec(t *testing.T) {
	clientset, requests := fakeSATokenClient(func(call int, namespace string, _ *authenticationv1.TokenRequest) (string, time.Time, error) {
		assert.Equal(t, utils.DefaultSandboxDeployNamespace, namespace, "the namespace is fixed and not configurable")
		return "tok-1", time.Now().Add(time.Hour), nil
	})
	s := &saTokenSource{clientset: clientset, saName: "sandbox-manager"}

	_, _, err := s.fetch(context.Background())
	require.NoError(t, err)

	got := requests()
	require.Len(t, got, 1)
	spec := got[0].Spec
	assert.Equal(t, []string{DefaultTokenAudience}, spec.Audiences)
	require.NotNil(t, spec.ExpirationSeconds)
	assert.EqualValues(t, tokenExpirationSeconds, *spec.ExpirationSeconds)
	// A sandbox-manager pod outside the user cluster is not an object there, so it cannot be a binding
	// target and no placeholder may be created for it.
	assert.Nil(t, spec.BoundObjectRef)
}

// ---------------------------------------------------------------------------
// start: first credential gates startup
// ---------------------------------------------------------------------------

func TestSATokenSource_Start(t *testing.T) {
	clientset, requests := fakeSATokenClient(func(int, string, *authenticationv1.TokenRequest) (string, time.Time, error) {
		return "", time.Time{}, errors.New("forbidden")
	})
	s := &saTokenSource{clientset: clientset, saName: "sa"}

	err := s.start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to request identity provider service account token")
	assert.Len(t, requests(), 1, "startup must not retry in the background")

	_, tokenErr := s.token()
	require.Error(t, tokenErr, "no credential may be published after a failed startup")

	t.Run("valid first credential is published", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clientset, _ := fakeSATokenClient(func(int, string, *authenticationv1.TokenRequest) (string, time.Time, error) {
			return "tok-1", time.Now().Add(time.Hour), nil
		})
		s := &saTokenSource{clientset: clientset, saName: "sa"}

		require.NoError(t, s.start(ctx))
		token, err := s.token()
		require.NoError(t, err)
		assert.Equal(t, "tok-1", token)
	})
}

// ---------------------------------------------------------------------------
// run: background refresh ownership
// ---------------------------------------------------------------------------

func TestSATokenSource_Run_RefreshesBeforeExpiration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var firstExpiration time.Time
	refreshRequested := make(chan time.Time, 1)
	clientset, _ := fakeSATokenClient(func(call int, _ string, _ *authenticationv1.TokenRequest) (string, time.Time, error) {
		if call == 0 {
			firstExpiration = time.Now().Add(3 * time.Second)
			return "tok-1", firstExpiration, nil
		}
		if call == 1 {
			refreshRequested <- time.Now()
		}
		return fmt.Sprintf("tok-%d", call+1), time.Now().Add(3 * time.Second), nil
	})
	s := &saTokenSource{clientset: clientset, saName: "sa"}
	require.NoError(t, s.start(ctx))

	first, err := s.token()
	require.NoError(t, err)
	assert.Equal(t, "tok-1", first)

	select {
	case requestedAt := <-refreshRequested:
		assert.True(t, requestedAt.Before(firstExpiration), "refresh must be requested before the cached token expires")
	case <-time.After(10 * time.Second):
		t.Fatal("the background owner did not request a refresh")
	}

	require.Eventually(t, func() bool {
		got, err := s.token()
		return err == nil && got != first
	}, 10*time.Second, 20*time.Millisecond, "the background owner must replace the credential without a caller request")
}

func TestSATokenSource_Run_FailedRefreshKeepsOldExpiration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientset, requests := fakeSATokenClient(func(call int, _ string, _ *authenticationv1.TokenRequest) (string, time.Time, error) {
		if call == 0 {
			return "tok-1", time.Now().Add(1200 * time.Millisecond), nil
		}
		return "", time.Time{}, errors.New("user cluster API unavailable")
	})
	s := &saTokenSource{clientset: clientset, saName: "sa"}
	require.NoError(t, s.start(ctx))

	s.mu.RLock()
	initial := s.cachedExpiration
	s.mu.RUnlock()

	require.Eventually(t, func() bool { return len(requests()) >= 2 }, 10*time.Second, 20*time.Millisecond)

	s.mu.RLock()
	assert.Equal(t, initial, s.cachedExpiration, "a failed refresh must not extend the cached expiration")
	s.mu.RUnlock()

	// Once the old credential genuinely expires, callers get a local error instead of an
	// expired token or an anonymous request.
	require.Eventually(t, func() bool {
		_, err := s.token()
		return err != nil
	}, 10*time.Second, 20*time.Millisecond)
}

func TestSATokenSource_Run_RecoversWithoutRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientset, _ := fakeSATokenClient(func(call int, _ string, _ *authenticationv1.TokenRequest) (string, time.Time, error) {
		switch call {
		case 0:
			return "tok-1", time.Now().Add(1200 * time.Millisecond), nil
		case 1:
			return "", time.Time{}, errors.New("user cluster API unavailable")
		default:
			return fmt.Sprintf("tok-%d", call+1), time.Now().Add(2 * time.Second), nil
		}
	})
	s := &saTokenSource{clientset: clientset, saName: "sa"}
	require.NoError(t, s.start(ctx))

	require.Eventually(t, func() bool {
		got, err := s.token()
		return err == nil && got != "tok-1"
	}, 20*time.Second, 20*time.Millisecond, "a later success must resume service without a restart")
}

func TestSATokenSource_Run_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	clientset, requests := fakeSATokenClient(func(call int, _ string, _ *authenticationv1.TokenRequest) (string, time.Time, error) {
		return fmt.Sprintf("tok-%d", call+1), time.Now().Add(300 * time.Millisecond), nil
	})
	s := &saTokenSource{clientset: clientset, saName: "sa"}
	require.NoError(t, s.start(ctx))

	require.Eventually(t, func() bool { return len(requests()) >= 2 }, 10*time.Second, 10*time.Millisecond)

	cancel()
	// Absorb an iteration that was already past its cancellation check before settling.
	time.Sleep(200 * time.Millisecond)
	settled := len(requests())

	time.Sleep(1500 * time.Millisecond)
	assert.Equal(t, settled, len(requests()), "no refresh may be issued after the hook context is canceled")
}

// capturingContext returns a context carrying a verbose logr.Logger that records
// everything logged through it, plus a race-free accessor for the captured text. It
// stands in for an operator who raised the process verbosity: code under test must not
// rely on that verbosity staying low to keep credentials out of logs.
func capturingContext(verbosity int) (context.Context, func() string) {
	var mu sync.Mutex
	var logged strings.Builder

	logger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged.WriteString(args)
		logged.WriteByte('\n')
	}, funcr.Options{Verbosity: verbosity})

	return logr.NewContext(context.Background(), logger), func() string {
		mu.Lock()
		defer mu.Unlock()
		return logged.String()
	}
}

// TestDescribeRemoteFailure_TransportCategories covers the branches a real API server
// answer cannot reach: a client-side deadline, a cancellation and a transport failure.
// HTTP status extraction and response body redaction are covered end to end by
// TestCredentialReadFailureErrorsStaySafe.
//
// All three arrive wrapped in a *url.Error, which formats the full request URL into its
// message. The design forbids echoing a raw address, so the URL must not survive either.
func TestDescribeRemoteFailure_TransportCategories(t *testing.T) {
	const remoteAddress = "https://user-cluster.example.internal"
	dialErr := &url.Error{
		Op:  "Post",
		URL: remoteAddress + "/api/v1/namespaces/" + utils.DefaultSandboxDeployNamespace + "/secrets/cert",
		Err: errors.New("dial tcp: connection refused"),
	}

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "a client side deadline is reported as a timeout",
			err:  &url.Error{Op: "Post", URL: remoteAddress, Err: context.DeadlineExceeded},
			want: "the call timed out before a response arrived",
		},
		{
			name: "a canceled call is reported as canceled",
			err:  &url.Error{Op: "Post", URL: remoteAddress, Err: context.Canceled},
			want: "the call was canceled before a response arrived",
		},
		{
			name: "a transport failure keeps only its category",
			err:  dialErr,
			want: "the call failed before a response was available",
		},
		{
			// A local transport-construction failure and a dial failure are not
			// distinguishable here, and neither may be reported as the server having
			// stayed silent.
			name: "a local failure before any request keeps the same category",
			err:  errors.New("invalid runtime TLS configuration: x509: certificate signed by unknown authority"),
			want: "the call failed before a response was available",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := describeRemoteFailure(tt.err, 0)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// Privacy: the credential must never reach a logger
// ---------------------------------------------------------------------------

func TestSATokenSource_NeverLogsTheCredential(t *testing.T) {
	verboseCtx, captured := capturingContext(9)
	ctx, cancel := context.WithCancel(verboseCtx)
	defer cancel()

	clientset, _ := fakeSATokenClient(func(call int, _ string, _ *authenticationv1.TokenRequest) (string, time.Time, error) {
		return fmt.Sprintf("secret-token-%d", call+1), time.Now().Add(300 * time.Millisecond), nil
	})
	s := &saTokenSource{clientset: clientset, saName: "sa"}
	require.NoError(t, s.start(ctx))

	require.Eventually(t, func() bool { return captured() != "" }, 10*time.Second, 10*time.Millisecond,
		"the refresh loop must log before checking that credentials stay private")
	cancel()

	output := captured()
	assert.Contains(t, output, utils.DefaultSandboxDeployNamespace+"/sa", "safe diagnostic context is retained")
	assert.NotContains(t, output, "secret-token-", "a ServiceAccount token must never reach logs")
}
