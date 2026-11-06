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
	"math"
	"sync"
	"time"

	"github.com/go-logr/logr"
	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/utils"
)

// EnvIdentityProviderTokenServiceAccount selects the TokenRequest credential mode for
// the sandbox-manager Secret-backed identity provider. It holds only a ServiceAccount
// name; the namespace is fixed to the user cluster's sandbox-system.
//
// When empty the provider keeps reading the kubelet-projected token file on every
// request. When set, a sandbox-manager running outside the user cluster cannot rely on
// a projection into its own pod, so it requests short-lived tokens from the user cluster
// API and refreshes them in memory instead.
const EnvIdentityProviderTokenServiceAccount = "IDENTITY_PROVIDER_TOKEN_SERVICE_ACCOUNT"

// EnvIdentityProviderTokenAudience overrides the audience requested for the
// ServiceAccount token. It must match the audience the identity provider accepts.
const EnvIdentityProviderTokenAudience = "IDENTITY_PROVIDER_TOKEN_AUDIENCE" //nolint:gosec // G101 false positive: an env var name, not a credential

// TokenRequest parameters. The namespace is deliberately not exposed as configuration:
// a namespace override would let a deployment point the credential at a ServiceAccount
// outside the manager's own namespace.
const (
	// DefaultTokenAudience is the audience requested for the ServiceAccount token when
	// EnvIdentityProviderTokenAudience is not set.
	DefaultTokenAudience = "agent-identity-provider" //nolint:gosec // G101 false positive: a JWT audience identifier, not a credential

	// tokenExpirationSeconds is the requested lifetime. The API server may grant less,
	// so the refresh schedule always uses the returned expiration instead of this value.
	tokenExpirationSeconds = 3600
)

// Timing bounds for credential acquisition and background refresh.
const (
	// tokenRequestTimeout bounds a single TokenRequest, including the one performed
	// during startup.
	tokenRequestTimeout = 30 * time.Second

	// refreshNumerator and refreshDenominator set the refresh point to 4/5 (80%) of the
	// granted lifetime.
	refreshDenominator = 5
	refreshNumerator   = 4

	// minRefreshDelay floors the refresh wait so a pathologically short granted lifetime
	// cannot turn the refresh loop into a request storm against the user cluster API.
	minRefreshDelay = time.Second

	// minRefreshBackoff and maxRefreshBackoff bound the wait after a failed refresh.
	// The loop keeps retrying forever: a recovered user cluster API must resume service
	// without a process restart.
	minRefreshBackoff = 1 * time.Second
	maxRefreshBackoff = 60 * time.Second
)

// saTokenSource owns the identity provider calling credential obtained through the user
// cluster's ServiceAccount token subresource.
//
// One source exists per process and holds a single background refresh owner, so at most
// one TokenRequest is ever in flight. Business callers go through token, which reads a
// snapshot and never waits on the network nor requests a token of its own. Replicas do
// not share tokens and need no leader election or distributed lock.
//
// A failed refresh leaves the cached credential untouched: the old expiration is never
// extended, so token starts reporting an error as soon as that credential genuinely
// expires and recovers by itself on the next success.
type saTokenSource struct {
	clientset kubernetes.Interface
	saName    string
	// audience is the audience requested for the token. DefaultTokenAudience is used
	// when empty.
	audience string

	mu               sync.RWMutex
	cachedToken      string
	cachedExpiration time.Time
}

// fetch performs one bounded TokenRequest and validates the result. Only a response that
// carries a non-empty token with a future expiration is accepted, so a partially valid
// response can never replace a cached credential that is still usable.
func (s *saTokenSource) fetch(ctx context.Context) (string, time.Time, error) {
	requestCtx, cancel := context.WithTimeout(quietContext(ctx), tokenRequestTimeout)
	defer cancel()

	expirationSeconds := int64(tokenExpirationSeconds)
	audience := s.audience
	if audience == "" {
		audience = DefaultTokenAudience
	}
	resp, err := s.clientset.CoreV1().ServiceAccounts(utils.DefaultSandboxDeployNamespace).CreateToken(
		requestCtx,
		s.saName,
		&authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{
				Audiences:         []string{audience},
				ExpirationSeconds: &expirationSeconds,
				// No BoundObjectRef: a sandbox-manager pod running outside the user
				// cluster is not an object there, so it cannot be a binding target and no placeholder is
				// created for it. The identity anchor stays the existing user cluster
				// ServiceAccount; the kubeconfig user is only the caller that requests
				// the token.
			},
		},
		metav1.CreateOptions{})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to request identity provider service account token for %s/%s: %s",
			utils.DefaultSandboxDeployNamespace, s.saName, describeRemoteFailure(err, 0))
	}

	sa := utils.DefaultSandboxDeployNamespace + "/" + s.saName
	if resp.Status.Token == "" {
		return "", time.Time{}, fmt.Errorf("identity provider service account token request for %s returned an empty token", sa)
	}
	if resp.Status.ExpirationTimestamp.IsZero() {
		return "", time.Time{}, fmt.Errorf("identity provider service account token request for %s returned no expiration", sa)
	}
	expiration := resp.Status.ExpirationTimestamp.Time
	if !time.Now().Before(expiration) {
		return "", time.Time{}, fmt.Errorf("identity provider service account token request for %s returned an already expired token", sa)
	}
	return resp.Status.Token, expiration, nil
}

func (s *saTokenSource) publish(token string, expiration time.Time) {
	s.mu.Lock()
	s.cachedToken = token
	s.cachedExpiration = expiration
	s.mu.Unlock()
}

// token returns the cached credential snapshot. It matches the
// IdentityProviderOptions.TokenFunc signature so both sandbox-manager Secret modes share
// one transport path.
//
// An expired credential is reported as a local error rather than being sent: the identity
// provider would answer with an authorization failure that hides the real cause, and the
// request would leave the process carrying an identity the API server no longer honors.
func (s *saTokenSource) token() (string, error) {
	s.mu.RLock()
	token, expiration := s.cachedToken, s.cachedExpiration
	s.mu.RUnlock()

	if token == "" {
		return "", errors.New("identity provider service account token has not been issued")
	}
	if !time.Now().Before(expiration) {
		return "", fmt.Errorf("identity provider service account token expired at %s",
			expiration.UTC().Format(time.RFC3339))
	}
	return token, nil
}

// start acquires the first credential and then hands all later refreshes to a single
// background goroutine bounded by ctx.
//
// Startup must not open the business endpoints without a usable credential, so any
// failure of the first request is returned to the caller instead of being retried in the
// background.
func (s *saTokenSource) start(ctx context.Context) error {
	token, expiration, err := s.fetch(ctx)
	if err != nil {
		return err
	}
	s.publish(token, expiration)
	go s.run(ctx, expiration)
	return nil
}

// run is the sole refresh owner for this process. expiration is the lifetime granted to
// the credential that start already published; later iterations use the value returned by
// their own request, so a server that grants less than tokenExpirationSeconds still
// refreshes in time.
func (s *saTokenSource) run(ctx context.Context, expiration time.Time) {
	logger := klog.FromContext(ctx).WithValues(
		"serviceAccount", utils.DefaultSandboxDeployNamespace+"/"+s.saName)

	var failures int
	backoff := refreshBackoff
	delay := nextRefreshDelay(time.Until(expiration))
	for {
		if !waitFor(ctx, delay) {
			logger.Info("identity provider token refresh stopped",
				"reason", "context canceled", "consecutiveFailures", failures)
			return
		}

		token, next, err := s.fetch(ctx)
		if err != nil {
			failures++
			delay = backoff.Step()
			logger.Error(err, "identity provider token refresh failed",
				"consecutiveFailures", failures, "retryIn", delay)
			continue
		}

		failures = 0
		backoff = refreshBackoff
		delay = nextRefreshDelay(time.Until(next))
		s.publish(token, next)
		logger.Info("identity provider token refreshed",
			"expiresInSeconds", int64(time.Until(next).Seconds()))
	}
}

// waitFor blocks for d and reports whether it fully elapsed; a false return means ctx
// is done.
func waitFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// nextRefreshDelay returns how long to wait before refreshing a credential whose remaining
// lifetime is available. The refresh point is a fraction of the granted lifetime rather
// than the requested one, because the API server may grant less than requested. The floor
// below is capped by the remaining lifetime, so a refresh is never scheduled past
// expiration.
func nextRefreshDelay(available time.Duration) time.Duration {
	if available <= 0 {
		return 0
	}
	delay := available / refreshDenominator * refreshNumerator

	// The floor keeps a pathologically short granted lifetime from turning the loop into
	// a request storm against the user cluster API.
	if floor := min(minRefreshDelay, available); delay < floor {
		delay = floor
	}
	return delay
}

// refreshBackoff is the template for the wait after consecutive refresh failures:
// minRefreshBackoff doubling up to maxRefreshBackoff. Each run loop steps its own copy.
var refreshBackoff = wait.Backoff{
	Duration: minRefreshBackoff,
	Factor:   2,
	Cap:      maxRefreshBackoff,
	Steps:    math.MaxInt32,
}

// quietContext suppresses client-go request and response body logging for credential
// reads. client-go logs bodies through the logger carried in the context, so relying on
// the process verbosity staying low is not enough: a Secret Get body holds the client
// private key and a TokenRequest body holds the issued token.
func quietContext(ctx context.Context) context.Context {
	return logr.NewContext(ctx, logr.Discard())
}

// describeRemoteFailure reduces a failed remote call to the diagnostic detail the design
// permits: an HTTP status and, for a Kubernetes API answer, its structured reason, or else
// a timeout, cancellation or bare transport category. statusCode is the status the caller
// already observed, or 0 to derive it from err.
//
// err is classified but never formatted into the result. client-go copies up to 2 KiB of a
// response body verbatim into its error when an intermediary answers with text that is not
// a metav1.Status, and the runtime files API copies up to 1 KiB of its own error body. Both
// peers may sit behind intermediaries that can echo request material back, and every error
// built here is logged — where that material is a ServiceAccount token, a client private
// key or a sandbox access token.
func describeRemoteFailure(err error, statusCode int) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "the call timed out before a response arrived"
	}
	if errors.Is(err, context.Canceled) {
		return "the call was canceled before a response arrived"
	}

	if statusCode == 0 {
		var apiStatus apierrors.APIStatus
		if errors.As(err, &apiStatus) {
			statusCode = int(apiStatus.Status().Code)
		}
	}
	if statusCode == 0 {
		// A zero status covers both a genuine transport failure and a call that never
		// left this process — an invalid runtime TLS bundle or an unresolvable runtime
		// address fails before a request is built. The wording must not blame the server
		// for that second case, and the two are not distinguishable from here.
		return "the call failed before a response was available"
	}
	// Reason is a bounded metav1 enum. Status.Message is the free-text field that carries
	// the remote body, and it is the field the design requires dropping.
	if reason := apierrors.ReasonForError(err); reason != metav1.StatusReasonUnknown {
		return fmt.Sprintf("the server returned status %d (%s)", statusCode, reason)
	}
	return fmt.Sprintf("the server returned status %d", statusCode)
}
