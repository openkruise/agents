package keys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

var (
	KeySecretName    = "e2b-key-store"
	AdminKeyID       uuid.UUID
	generateUUID     = uuid.New
	marshalAPIKey    = json.Marshal
	newRefreshTicker = func() *time.Ticker {
		return time.NewTicker(10 * time.Minute)
	}
)

func init() {
	AdminKeyID = uuid.MustParse("550e8400-e29b-41d4-a716-446655440000") // no means, just a const
}

// secretKeyStorage is a simple implement for api-key storage using k8s secret as storage backend.
// It is only for demo purpose.
type secretKeyStorage struct {
	Namespace string
	AdminKey  string

	Client kubernetes.Interface
	stop   chan struct{}
	done   chan struct{}

	stopOnce sync.Once

	idxByKey sync.Map
	idxByID  sync.Map
}

func NewSecretKeyStorage(client kubernetes.Interface, namespace, adminKey string) KeyStorage {
	return &secretKeyStorage{
		Namespace: namespace,
		AdminKey:  adminKey,
		Client:    client,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

func (k *secretKeyStorage) Init(ctx context.Context) error {
	log := klog.FromContext(ctx)
	log.Info("ensuring api-key store secret")

	secret, err := k.Client.CoreV1().Secrets(k.Namespace).Get(ctx, KeySecretName, metav1.GetOptions{})
	if err != nil {
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
		}
		if err = k.retryUpdateSecret(ctx, AdminKeyID.String(), adminKey); err != nil && !apierrors.IsConflict(err) {
			return err
		} else if err == nil {
			log.Info("create admin key success", "key", adminKey.Key)
		}
	}

	return k.refresh(ctx)
}

func (k *secretKeyStorage) refresh(ctx context.Context) error {
	log := klog.FromContext(ctx)
	log.Info("refreshing api-key store")
	secret, err := k.Client.CoreV1().Secrets(k.Namespace).Get(ctx, KeySecretName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	var ids, keys = sets.NewString(), sets.NewString()
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
	}
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
	return nil
}

func (k *secretKeyStorage) Run() {
	// Capture newRefreshTicker synchronously in the calling goroutine to avoid
	// a data race between the background goroutine reading newRefreshTicker and
	// test cleanup code writing to it after the test function returns.
	tickerFactory := newRefreshTicker
	go func() {
		defer close(k.done)
		ticker := tickerFactory()
		ctx := logs.NewContext()
		log := klog.FromContext(ctx)
		for {
			select {
			case <-ticker.C:
				if err := k.refresh(ctx); err != nil {
					log.Error(err, "failed to refresh key store")
				}
			case <-k.stop:
				ticker.Stop()
				log.Info("api-key refreshing stopped")
				return
			}
		}
	}()
}

// Stop signals the background refresh goroutine to exit and waits for it to finish.
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

func (k *secretKeyStorage) updateSecret(ctx context.Context, id string, apiKey *models.CreatedTeamAPIKey) error {
	secret, err := k.Client.CoreV1().Secrets(k.Namespace).Get(ctx, KeySecretName, metav1.GetOptions{})
	if err != nil {
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
	_, err = k.Client.CoreV1().Secrets(k.Namespace).Update(ctx, secret, metav1.UpdateOptions{})
	return err
}

func (k *secretKeyStorage) storeKey(apiKey *models.CreatedTeamAPIKey) {
	k.idxByKey.Store(apiKey.Key, apiKey)
	k.idxByID.Store(apiKey.ID.String(), apiKey)
}

func (k *secretKeyStorage) CreateKey(ctx context.Context, user *models.CreatedTeamAPIKey, name string) (*models.CreatedTeamAPIKey, error) {
	log := klog.FromContext(ctx).WithValues("name", name).V(consts.DebugLogLevel)
	if name == "" || user == nil {
		return nil, errors.New("api-key name and user are required")
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
		Name:      name,
		CreatedBy: &models.TeamUser{
			ID: user.ID,
		},
	}

	log.Info("api-key generated", "key", apiKey)
	if err := k.retryUpdateSecret(ctx, newID.String(), apiKey); err != nil {
		log.Error(err, "failed to update api-key")
		return nil, err
	}
	k.storeKey(apiKey)
	return apiKey, nil
}

func (k *secretKeyStorage) DeleteKey(ctx context.Context, key *models.CreatedTeamAPIKey) error {
	if key == nil {
		return nil
	}
	err := k.retryUpdateSecret(ctx, key.ID.String(), nil)
	if err != nil {
		return err
	}
	k.idxByKey.Delete(key.Key)
	k.idxByID.Delete(key.ID.String())
	return nil
}

func (k *secretKeyStorage) ListByOwner(_ context.Context, owner uuid.UUID) ([]*models.TeamAPIKey, error) {
	var result []*models.TeamAPIKey
	k.idxByID.Range(func(_, value any) bool {
		apikey := value.(*models.CreatedTeamAPIKey)
		if apikey.ID == owner || apikey.CreatedBy.ID == owner {
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
