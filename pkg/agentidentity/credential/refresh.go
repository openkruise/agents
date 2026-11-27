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

package credential

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openkruise/agents/pkg/identity/delegation"
)

// ErrReauthorizationRequired means the delegation exists but can no longer
// produce an access token: the provider refused its refresh token, or it never
// had one and its access token is expiring. Only the user can restore it, so
// section 6.5 answers with AuthorizationRequired, the same result a user who
// never authorized gets.
var ErrReauthorizationRequired = errors.New("credential: delegation needs reauthorization")

const (
	// refreshMargin is how far ahead of expiry an access token is replaced. A
	// token handed to an agent should still be good for the call it was asked
	// for, so one about to lapse is refreshed rather than returned.
	refreshMargin = 5 * time.Minute

	// refreshTimeout bounds one refresh: the token endpoint round trip and the
	// store writes around it. It belongs to the refresh rather than to any one
	// caller, because callers share it.
	refreshTimeout = 30 * time.Second

	// tokenEndpointTimeout bounds a single request to a provider.
	tokenEndpointTimeout = 10 * time.Second

	// cacheCatchUpInterval is how often a read that is known to be behind the
	// API server is retried.
	cacheCatchUpInterval = 50 * time.Millisecond

	// defaultRefusalGrace is how long a refused refresh token is held before
	// the refusal is acted on; see confirmRefusal.
	defaultRefusalGrace = 2 * time.Second
)

// OAuth2 error codes meaning the refresh token itself is dead.
const (
	// errorInvalidGrant is RFC 6749 section 5.2's code for an expired, revoked
	// or already-used refresh token.
	errorInvalidGrant = "invalid_grant"
	// errorBadRefreshToken is GitHub's code for the same thing, which it uses
	// instead of invalid_grant.
	errorBadRefreshToken = "bad_refresh_token"
)

// Refresher hands out delegations whose access tokens are usable, refreshing
// them first when they are about to expire.
type Refresher struct {
	reader       client.Reader
	store        delegation.DelegationStore
	httpClient   *http.Client
	refusalGrace time.Duration
	flights      singleflight.Group
}

// NewRefresher returns a Refresher that loads providers through reader and
// keeps delegations in store.
func NewRefresher(reader client.Reader, store delegation.DelegationStore) *Refresher {
	return &Refresher{
		reader: reader,
		store:  store,
		httpClient: &http.Client{
			Timeout: tokenEndpointTimeout,
			// A redirect is refused rather than followed. Go re-sends a POST
			// body on a 307 or 308, and this body carries the client secret and
			// the refresh token, so following one would hand both to wherever
			// the token endpoint pointed.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		refusalGrace: defaultRefusalGrace,
	}
}

// Token returns the delegation at key with an access token that stays usable
// for at least refreshMargin, refreshing it first if it would not.
//
// This is section 6.5. A token that is still good is returned as stored. One
// near expiry is refreshed in a single flight per delegation and written back
// with compare-and-swap. If there is no refresh token, or the provider rejects
// it and no other replica turns out to have refreshed it meanwhile, the
// delegation is marked NeedsReauthorization and Token returns
// ErrReauthorizationRequired. A delegation that does not exist returns
// delegation.ErrNotFound, and delegation.ErrConflict means it kept moving
// under this refresh and the caller may retry.
//
// The record returned is the caller's own. Checking scopes and authorizing
// the request remain the caller's job: Token decides only whether the stored
// credential can still be used.
func (r *Refresher) Token(ctx context.Context, key delegation.DelegationKey) (*delegation.Delegation, error) {
	record, err := r.store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if record.State == delegation.DelegationNeedsReauthorization {
		return nil, ErrReauthorizationRequired
	}
	if !needsRefresh(record) {
		return record, nil
	}

	// The flight runs detached from whichever caller started it. Otherwise one
	// caller giving up would fail the refresh for every caller sharing it, and
	// could abandon it after the provider had already spent the old refresh
	// token but before its replacement was stored, which leaves only the user
	// able to restore the delegation. Each caller still stops waiting when its
	// own context ends.
	results := r.flights.DoChan(delegation.DelegationSecretName(key), func() (any, error) {
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
		defer cancel()
		return r.refresh(flightCtx, key)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-results:
		if result.Err != nil {
			return nil, result.Err
		}
		// Every caller in the flight receives the same pointer.
		shared := result.Val.(*delegation.Delegation)
		own := *shared
		own.Scopes = slices.Clone(shared.Scopes)
		return &own, nil
	}
}

// refresh does the work of one flight.
func (r *Refresher) refresh(ctx context.Context, key delegation.DelegationKey) (*delegation.Delegation, error) {
	// Read again inside the flight. Another replica, or a flight that ended
	// just before this one began, may already have refreshed it. The store's
	// version is what keeps replicas consistent; this process's single flight
	// only saves duplicate work within it (section 8.3).
	record, err := r.store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if record.State == delegation.DelegationNeedsReauthorization {
		return nil, ErrReauthorizationRequired
	}
	if !needsRefresh(record) {
		return record, nil
	}
	if record.RefreshToken == "" {
		return r.requireReauthorization(ctx, record, "no refresh token")
	}

	provider, err := LoadProvider(ctx, r.reader,
		client.ObjectKey{Namespace: key.Namespace, Name: key.CredentialProvider})
	if err != nil {
		return nil, err
	}
	token, err := provider.refresh(context.WithValue(ctx, oauth2.HTTPClient, r.httpClient), record.RefreshToken)
	if err != nil {
		var rejected *oauth2.RetrieveError
		if !errors.As(err, &rejected) {
			return nil, fmt.Errorf("credential: refresh access token: %w", err)
		}
		if rejected.ErrorCode == errorInvalidGrant || rejected.ErrorCode == errorBadRefreshToken {
			return r.confirmRefusal(ctx, record)
		}
		// Anything else, invalid_client or a 5xx among them, says nothing about
		// the user's grant and must not cost them their authorization.
		return nil, fmt.Errorf("credential: refresh access token: %s", describeRejection(rejected))
	}

	refreshed := *record
	refreshed.AccessToken = token.AccessToken
	// x/oauth2 carries the old refresh token forward when the response leaves
	// it out, which RFC 6749 section 6 permits.
	refreshed.RefreshToken = token.RefreshToken
	// Zero when the response has no expires_in, which means the token does not
	// expire, the same reading needsRefresh gives it.
	refreshed.AccessTokenExpiresAt = token.Expiry
	if scopes := grantedScopes(token); len(scopes) > 0 {
		refreshed.Scopes = scopes
	}
	refreshed.UpdatedAt = time.Now()
	stored, err := r.storeRefreshed(ctx, &refreshed)
	if err != nil {
		return nil, err
	}
	log.FromContext(ctx).V(4).Info("Refreshed a delegation's access token",
		delegationLogValues(key, "expiresAt", stored.AccessTokenExpiresAt)...)
	return stored, nil
}

// storeRefreshed writes a refreshed delegation, settling a lost race.
//
// A conflict means another writer moved the record after this refresh read
// it. If the record is gone it was withdrawn, and a refresh must not bring it
// back, which is why the retry below can only update and never create. If it
// is Active and usable, the other writer - a callback, or another replica - is
// at least as current, and its record wins. Otherwise it is
// NeedsReauthorization, written by a refresh that lost to this one: providers
// such as GitHub spend a refresh token on first use, so the replica that
// refreshed second was refused and marked the delegation, while this refresh
// holds the working replacement. That replacement is written over it.
func (r *Refresher) storeRefreshed(ctx context.Context,
	refreshed *delegation.Delegation) (*delegation.Delegation, error) {
	err := r.store.Put(ctx, refreshed)
	if !errors.Is(err, delegation.ErrConflict) {
		if err != nil {
			return nil, err
		}
		return refreshed, nil
	}

	current, err := r.readPast(ctx, refreshed.Key, refreshed.Version)
	if err != nil {
		return nil, err
	}
	if current.State == delegation.DelegationActive && !needsRefresh(current) {
		return current, nil
	}
	refreshed.Version = current.Version
	if err := r.store.Put(ctx, refreshed); err != nil {
		return nil, err
	}
	return refreshed, nil
}

// requireReauthorization marks the delegation NeedsReauthorization and
// returns ErrReauthorizationRequired, unless another writer got there first
// with something usable.
//
// The tokens are cleared as it is marked. Neither can produce an access token
// any more, and a record only the user can restore has no reason to keep
// holding them.
func (r *Refresher) requireReauthorization(ctx context.Context, record *delegation.Delegation,
	reason string) (*delegation.Delegation, error) {
	marked := *record
	marked.State = delegation.DelegationNeedsReauthorization
	marked.AccessToken = ""
	marked.RefreshToken = ""
	marked.AccessTokenExpiresAt = time.Time{}
	marked.UpdatedAt = time.Now()
	err := r.store.Put(ctx, &marked)
	if err == nil {
		log.FromContext(ctx).Info("Delegation needs reauthorization",
			delegationLogValues(record.Key, "reason", reason)...)
		return nil, ErrReauthorizationRequired
	}
	if !errors.Is(err, delegation.ErrConflict) {
		return nil, err
	}

	// Another writer moved first. What it stored decides.
	current, err := r.readPast(ctx, record.Key, record.Version)
	if err != nil {
		return nil, err
	}
	return settledBy(current)
}

// confirmRefusal acts on the provider refusing record's refresh token, once
// the refusal has been given refusalGrace to turn out to be a lost race.
//
// A provider that spends a refresh token on first use, as GitHub does,
// refuses it to every replica after the one that used it, so a refusal means
// either that the grant is gone or that another replica has just refreshed.
// That replica writes its result within moments of its own response, which
// the provider sent before this one. Marking at once would answer every caller
// here with AuthorizationRequired while a working token was about to land,
// sending the user to authorize again for nothing; waiting briefly for the
// record to move costs a grant that really is gone only the grace period,
// ahead of an authorization flow that takes far longer.
func (r *Refresher) confirmRefusal(ctx context.Context,
	record *delegation.Delegation) (*delegation.Delegation, error) {
	graceCtx, cancel := context.WithTimeout(ctx, r.refusalGrace)
	defer cancel()
	current, err := r.readPast(graceCtx, record.Key, record.Version)
	switch {
	case err == nil:
		return settledBy(current)
	case graceCtx.Err() != nil && ctx.Err() == nil:
		// Nothing moved it, so nothing raced this refresh: the grant is gone.
		return r.requireReauthorization(ctx, record, "the provider rejected the refresh token")
	default:
		return nil, err
	}
}

// settledBy is what another writer's record means for a refresh that was
// refused, or whose mark lost: a working token is used, a mark stands, and a
// record still due for refresh is a conflict the caller may retry.
func settledBy(current *delegation.Delegation) (*delegation.Delegation, error) {
	switch {
	case current.State == delegation.DelegationNeedsReauthorization:
		return nil, ErrReauthorizationRequired
	case !needsRefresh(current):
		return current, nil
	default:
		return nil, fmt.Errorf("%w: the delegation changed while it was being refreshed", delegation.ErrConflict)
	}
}

// readPast reads the delegation once a read shows a version other than past,
// or gives up when ctx ends.
//
// After a write the store refused because it was made against past, the
// refusal came from the API server and proves a newer version exists, so a
// read still showing past is the informer cache lagging, not the record.
// Deciding on that read would be deciding on a record already replaced, and
// after a lost race it can throw away the only working refresh token.
// confirmRefusal also uses it to wait, briefly, for a version that may never
// come.
func (r *Refresher) readPast(ctx context.Context, key delegation.DelegationKey,
	past string) (*delegation.Delegation, error) {
	for {
		current, err := r.store.Get(ctx, key)
		if err != nil || current.Version != past {
			return current, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("credential: wait for the delegation to move past version %s: %w",
				past, ctx.Err())
		case <-time.After(cacheCatchUpInterval):
		}
	}
}

// needsRefresh reports whether record's access token would lapse within
// refreshMargin, or is missing outright.
//
// A zero expiry means the provider gave the token no lifetime, which is how
// GitHub OAuth App tokens arrive, so it never needs refreshing.
func needsRefresh(record *delegation.Delegation) bool {
	if record.AccessToken == "" {
		return true
	}
	if record.AccessTokenExpiresAt.IsZero() {
		return false
	}
	return !time.Now().Add(refreshMargin).Before(record.AccessTokenExpiresAt)
}

// grantedScopes reads the scope a token response reports, or nil when it
// reports none.
//
// RFC 6749 separates scopes with spaces and GitHub with commas, so both are
// accepted. An absent or empty scope means the grant is unchanged, which RFC
// 6749 section 5.1 allows a server to signal by leaving it out.
func grantedScopes(token *oauth2.Token) []string {
	raw, _ := token.Extra("scope").(string)
	scopes := strings.FieldsFunc(raw, func(r rune) bool { return r == ' ' || r == ',' })
	if len(scopes) == 0 {
		return nil
	}
	slices.Sort(scopes)
	return slices.Compact(scopes)
}

// describeRejection summarizes a token endpoint's refusal by its status and
// error code alone. RetrieveError's own message includes the response body,
// which is not ours to vouch for: some providers echo the request back, and
// the request carried the refresh token and the client secret.
func describeRejection(rejected *oauth2.RetrieveError) string {
	description := "the provider refused the request"
	if rejected.Response != nil {
		description = "the provider answered " + rejected.Response.Status
	}
	if rejected.ErrorCode != "" {
		description += " with error " + strconv.Quote(rejected.ErrorCode)
	}
	return description
}

// delegationLogValues identifies a delegation in a log line by its opaque
// record name and the resources it names, never by the user it belongs to.
func delegationLogValues(key delegation.DelegationKey, extra ...any) []any {
	return append([]any{
		"delegation", delegation.DelegationSecretName(key),
		"namespace", key.Namespace,
		"agentIdentity", key.AgentIdentity,
		"credentialProvider", key.CredentialProvider,
	}, extra...)
}
