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

package keys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/utils"
)

var (
	KeySecretName = "e2b-key-store" // #nosec G101 -- resource name, not a credential
	AdminKeyID    uuid.UUID
	generateUUID  = uuid.New
	marshalAPIKey = json.Marshal
)

const secretKeyStorageRefreshInterval = 10 * time.Minute

func init() {
	AdminKeyID = uuid.MustParse("550e8400-e29b-41d4-a716-446655440000") // no means, just a const
}

// secretKeyStorage is a simple implement for api-key storage using k8s secret as storage backend.
// It is only for demo purpose.
type secretKeyStorage struct {
	Namespace string
	AdminKey  string

	Client client.Client
	// APIReader is used for reading secrets before cache is started (e.g., during Init).
	APIReader client.Reader
	Cache     ctrlcache.Cache

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	refreshC chan struct{}
	wg       sync.WaitGroup

	refreshInterval time.Duration

	idxByKey  sync.Map
	idxByID   sync.Map
	idxByTeam sync.Map // teamName -> *models.Team
}

func NewSecretKeyStorage(client client.Client, apiReader client.Reader, cache ctrlcache.Cache, namespace, adminKey string) KeyStorage {
	return &secretKeyStorage{
		Namespace:       namespace,
		AdminKey:        adminKey,
		Client:          client,
		APIReader:       apiReader,
		Cache:           cache,
		stop:            make(chan struct{}),
		done:            make(chan struct{}),
		refreshC:        make(chan struct{}, 1),
		refreshInterval: secretKeyStorageRefreshInterval,
	}
}

func (k *secretKeyStorage) Init(ctx context.Context) error {
	log := klog.FromContext(ctx)
	log.Info("ensuring api-key store secret")

	secret := &corev1.Secret{}
	if err := k.APIReader.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: KeySecretName}, secret); err != nil {
		return err
	}

	// create admin key if needed
	// all replicas does the same operation, no matter who eventually wins the race.
	if _, ok := secret.Data[k.AdminKey]; !ok {
		adminKey := &models.CreatedTeamAPIKey{
			CreatedAt: time.Now(),
			ID:        AdminKeyID,
			Key:       k.AdminKey,
			Name:      "admin",
			Team:      models.AdminTeam(),
		}
		if err := k.retryUpdateSecret(ctx, AdminKeyID.String(), adminKey); err != nil && !apierrors.IsConflict(err) {
			return err
		} else if err == nil {
			log.Info("create admin key success", "id", adminKey.ID)
		}
	}

	return k.refresh(ctx, k.APIReader)
}

func (k *secretKeyStorage) refresh(ctx context.Context, reader client.Reader) error {
	log := klog.FromContext(ctx)
	log.Info("refreshing api-key store")
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: KeySecretName}, secret); err != nil {
		return err
	}
	// refresh is the only path that mutates the in-memory indexes. CreateKey
	// and DeleteKey intentionally only update the Secret and then wait for an
	// informer-backed refresh to publish the change locally. Mixing writer-side
	// index updates with queued stale informer refreshes can briefly revoke a
	// newly created key or restore a newly deleted key on the auth path.
	var ids, keys, teamNames = sets.NewString(), sets.NewString(), sets.NewString()
	for id, bytes := range secret.Data {
		var apiKey models.CreatedTeamAPIKey
		err := json.Unmarshal(bytes, &apiKey)
		if err != nil {
			log.Error(err, "failed to unmarshal api-key", "id", id)
			continue
		}
		k.storeKey(&apiKey)
		keys.Insert(apiKey.Key)
		ids.Insert(id)
		if apiKey.Team != nil {
			teamNames.Insert(apiKey.Team.Name)
		}
	}

	// clean up out-dated keys
	k.idxByKey.Range(func(key, _ any) bool {
		if !keys.Has(key.(string)) {
			k.idxByKey.Delete(key)
		}
		return true
	})
	k.idxByID.Range(func(id, _ any) bool {
		if !ids.Has(id.(string)) {
			k.idxByID.Delete(id)
		}
		return true
	})
	k.idxByTeam.Range(func(name, _ any) bool {
		if !teamNames.Has(name.(string)) {
			k.idxByTeam.Delete(name)
		}
		return true
	})
	return nil
}

// triggerRefresh uses refreshC, which is a buffered channel (cap=1), to coalesce refresh signals.
// Multiple events between refresh cycles are collapsed into a single refresh,
// preventing redundant Secret reads when the informer delivers a burst of events.
func (k *secretKeyStorage) triggerRefresh() {
	select {
	case k.refreshC <- struct{}{}:
	default:
	}
}

func (k *secretKeyStorage) refreshWorker(ctx context.Context) {
	defer k.wg.Done()
	log := klog.FromContext(ctx)
	refreshInterval := k.refreshInterval
	if refreshInterval <= 0 {
		refreshInterval = secretKeyStorageRefreshInterval
	}
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-k.refreshC:
			if err := k.refresh(ctx, k.Client); err != nil {
				log.Error(err, "failed to refresh key store")
			}
		case <-ticker.C:
			if err := k.refresh(ctx, k.Client); err != nil {
				log.Error(err, "failed to refresh key store")
			}
		case <-k.stop:
			return
		}
	}
}

func (k *secretKeyStorage) onSecretEvent(obj any) {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		if t, isTombstone := obj.(toolscache.DeletedFinalStateUnknown); isTombstone {
			secret, ok = t.Obj.(*corev1.Secret)
		}
		if !ok {
			return
		}
	}
	if secret.Namespace != k.Namespace || secret.Name != KeySecretName {
		return
	}
	k.triggerRefresh()
}

func (k *secretKeyStorage) Run() {
	ctx := logs.NewContext()
	log := klog.FromContext(ctx)

	informer, err := k.Cache.GetInformer(ctx, &corev1.Secret{})
	if err != nil {
		log.Error(err, "failed to get Secret informer; key store will not refresh")
		close(k.done)
		return
	}

	reg, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    k.onSecretEvent,
		UpdateFunc: func(_, newObj any) { k.onSecretEvent(newObj) },
		DeleteFunc: k.onSecretEvent,
	})
	if err != nil {
		log.Error(err, "failed to register Secret event handler")
		close(k.done)
		return
	}

	k.wg.Add(1)
	go k.refreshWorker(ctx)

	go func() {
		<-k.stop
		if removeErr := informer.RemoveEventHandler(reg); removeErr != nil {
			log.Error(removeErr, "failed to remove Secret event handler")
		}
		k.wg.Wait()
		close(k.done)
	}()
}

// Stop signals the background refresh worker to exit and waits for it to finish.
func (k *secretKeyStorage) Stop() {
	k.stopOnce.Do(func() {
		close(k.stop)
	})
	<-k.done
}

func (k *secretKeyStorage) LoadByKey(_ context.Context, key string) (*models.CreatedTeamAPIKey, bool) {
	value, ok := k.idxByKey.Load(key)
	if !ok {
		return nil, false
	}
	return value.(*models.CreatedTeamAPIKey), true
}

func (k *secretKeyStorage) LoadByID(_ context.Context, id string) (*models.CreatedTeamAPIKey, bool) {
	value, ok := k.idxByID.Load(id)
	if !ok {
		return nil, false
	}
	return value.(*models.CreatedTeamAPIKey), true
}

func (k *secretKeyStorage) retryUpdateSecret(ctx context.Context, id string, apiKey *models.CreatedTeamAPIKey) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return k.updateSecret(ctx, id, apiKey)
	})
}

func (k *secretKeyStorage) retryCreateKey(ctx context.Context, id string, apiKey *models.CreatedTeamAPIKey) (*models.CreatedTeamAPIKey, error) {
	var createdKey *models.CreatedTeamAPIKey
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		key, err := k.createKeyInSecret(ctx, id, apiKey)
		if err != nil {
			return err
		}
		createdKey = key
		return nil
	})
	if err != nil {
		return nil, err
	}
	return createdKey, nil
}

func (k *secretKeyStorage) updateSecret(ctx context.Context, id string, apiKey *models.CreatedTeamAPIKey) error {
	secret := &corev1.Secret{}
	if err := k.APIReader.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: KeySecretName}, secret); err != nil {
		return err
	}
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	if apiKey != nil {
		marshaled, err := marshalAPIKey(apiKey)
		if err != nil {
			return fmt.Errorf("failed to marshal api-key: %w", err)
		}
		secret.Data[id] = marshaled
	} else {
		delete(secret.Data, id)
	}
	return k.Client.Update(ctx, secret)
}

func (k *secretKeyStorage) createKeyInSecret(ctx context.Context, id string, apiKey *models.CreatedTeamAPIKey) (*models.CreatedTeamAPIKey, error) {
	secret := &corev1.Secret{}
	if err := k.APIReader.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: KeySecretName}, secret); err != nil {
		return nil, err
	}
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}

	keyToStore := *apiKey
	keyToStore.Team = cloneTeam(TeamForKey(apiKey))
	if existingTeam, ok := findTeamByNameInSecret(secret, keyToStore.Team.Name); ok {
		keyToStore.Team = existingTeam
	}

	marshaled, err := marshalAPIKey(&keyToStore)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal api-key: %w", err)
	}
	secret.Data[id] = marshaled
	if err := k.Client.Update(ctx, secret); err != nil {
		return nil, err
	}
	return &keyToStore, nil
}

// findTeamByNameInSecret scans the raw secret data instead of the in-memory cache
// (idxByTeam) to guarantee consistency with the exact secret revision being updated.
// During retryCreateKey, each retry re-reads the secret from the API server; using
// the cache here could miss teams created by other replicas between retries.
func findTeamByNameInSecret(secret *corev1.Secret, teamName string) (*models.Team, bool) {
	for _, bytes := range secret.Data {
		var apiKey models.CreatedTeamAPIKey
		if err := json.Unmarshal(bytes, &apiKey); err != nil {
			continue
		}
		team := TeamForKey(&apiKey)
		if team.Name == teamName {
			return cloneTeam(team), true
		}
	}
	return nil, false
}

func (k *secretKeyStorage) storeKey(apiKey *models.CreatedTeamAPIKey) {
	// after this, all old keys will be migrated to new keys in admin team
	if apiKey.Team == nil {
		apiKey.Team = models.AdminTeam()
	}
	k.idxByKey.Store(apiKey.Key, apiKey)
	k.idxByID.Store(apiKey.ID.String(), apiKey)
	k.idxByTeam.Store(apiKey.Team.Name, cloneTeam(apiKey.Team))
}

func (k *secretKeyStorage) CreateKey(ctx context.Context, key *models.CreatedTeamAPIKey, opts CreateKeyOptions) (*models.CreatedTeamAPIKey, error) {
	log := klog.FromContext(ctx).WithValues("name", opts.Name).V(utils.DebugLogLevel)
	teamName, err := validateCreateKeyOptions(key, opts)
	if err != nil {
		return nil, err
	}
	var team *models.Team
	callerTeam := TeamForKey(key)
	if callerTeam.Name == teamName {
		team = cloneTeam(callerTeam)
	} else if foundTeam, found, err := k.FindTeamByName(ctx, teamName); err != nil {
		return nil, err
	} else if found {
		team = foundTeam
	} else {
		team = &models.Team{ID: generateUUID(), Name: teamName}
	}

	var newID, newKey uuid.UUID
	for i := 0; i < 100; i++ {
		newID = generateUUID()
		newKey = generateUUID()
		_, ok1 := k.LoadByID(ctx, newID.String())
		_, ok2 := k.LoadByKey(ctx, newKey.String())
		if !ok1 && !ok2 {
			break
		}
		if i == 99 {
			return nil, errors.New("failed to generate unique api-key")
		}
	}

	apiKey := &models.CreatedTeamAPIKey{
		CreatedAt: time.Now(),
		ID:        newID,
		Key:       newKey.String(),
		Mask:      models.IdentifierMaskingDetails{},
		Name:      opts.Name,
		Team:      cloneTeam(team),
		CreatedBy: &models.TeamUser{
			ID: key.ID,
		},
	}

	log.Info("api-key generated", "id", apiKey.ID)
	createdKey, err := k.retryCreateKey(ctx, newID.String(), apiKey)
	if err != nil {
		log.Error(err, "failed to update api-key")
		return nil, err
	}
	return createdKey, nil
}

func (k *secretKeyStorage) DeleteKey(ctx context.Context, key *models.CreatedTeamAPIKey) error {
	if key == nil {
		return nil
	}
	if key.ID == AdminKeyID {
		return ErrAdminKeyUndeletable
	}
	err := k.retryUpdateSecret(ctx, key.ID.String(), nil)
	if err != nil {
		return err
	}
	return nil
}

func (k *secretKeyStorage) ListByOwnerTeam(_ context.Context, owner *models.CreatedTeamAPIKey) ([]*models.TeamAPIKey, error) {
	if owner == nil {
		return nil, nil
	}
	ownerTeam := TeamForKey(owner)
	var result []*models.TeamAPIKey
	k.idxByID.Range(func(_, value any) bool {
		apikey := value.(*models.CreatedTeamAPIKey)
		if TeamForKey(apikey).Name == ownerTeam.Name {
			result = append(result, &models.TeamAPIKey{
				CreatedAt: apikey.CreatedAt,
				ID:        apikey.ID,
				Mask:      apikey.Mask,
				Name:      apikey.Name,
				CreatedBy: apikey.CreatedBy,
				LastUsed:  apikey.LastUsed,
			})
		}
		return true
	})
	return result, nil
}

func (k *secretKeyStorage) ListTeams(_ context.Context, user *models.CreatedTeamAPIKey) ([]*models.ListedTeam, error) {
	if user == nil {
		return nil, nil
	}
	userTeam := TeamForKey(user)
	isAdmin := userTeam.Name == models.AdminTeamName
	teamsByName := map[string]*models.Team{}
	k.idxByID.Range(func(_, value any) bool {
		team := TeamForKey(value.(*models.CreatedTeamAPIKey))
		if !isAdmin && team.Name != userTeam.Name {
			return true
		}
		if _, exists := teamsByName[team.Name]; !exists {
			teamsByName[team.Name] = cloneTeam(team)
			// Non-admin users can only see their own team. Once it is
			// collected, further iteration is unnecessary. Guard with the
			// name match defensively in case the upstream filter ever stops
			// excluding other teams from this branch.
			if !isAdmin && team.Name == userTeam.Name {
				return false
			}
		}
		return true
	})
	result := make([]*models.ListedTeam, 0, len(teamsByName))
	for _, team := range teamsByName {
		result = append(result, listedTeam(team, team.Name == userTeam.Name))
	}
	return result, nil
}

func (k *secretKeyStorage) FindTeamByName(_ context.Context, teamName string) (*models.Team, bool, error) {
	value, ok := k.idxByTeam.Load(teamName)
	if !ok {
		return nil, false, nil
	}
	return cloneTeam(value.(*models.Team)), true, nil
}
