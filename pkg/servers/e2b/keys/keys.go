package keys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

var KeySecretName = "e2b-key-store"

// SecretKeyStorage is a simple implement for api-key storage using k8s secret as storage backend.
// It is only for demo purpose.
type SecretKeyStorage struct {
	Namespace string
	AdminKey  string

	Client kubernetes.Interface
	Stop   chan struct{}

	idxByKey sync.Map
	idxByID  sync.Map
}

func (k *SecretKeyStorage) Init(ctx context.Context) error {
	log := klog.FromContext(ctx)
	log.Info("ensuring api-key store secret")

	_, err := k.Client.CoreV1().Secrets(k.Namespace).Get(ctx, KeySecretName, metav1.GetOptions{})
	if err != nil && apierrors.IsNotFound(err) {
		if k.AdminKey == "" {
			k.AdminKey = uuid.NewString()
		}
		log.Info("api-key store secret not found, will create", "adminApiKey", k.AdminKey)
		adminID := uuid.New()
		adminKey := models.CreatedTeamAPIKey{
			CreatedAt: time.Now(),
			ID:        adminID,
			Key:       k.AdminKey,
			Mask:      models.IdentifierMaskingDetails{},
			Name:      "admin",
		}
		data, err := json.Marshal(adminKey)
		if err != nil {
			log.Error(err, "failed to marshal adminKey")
			return err
		}
		_, err = k.Client.CoreV1().Secrets(k.Namespace).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: KeySecretName,
			},
			Data: map[string][]byte{
				adminID.String(): data,
			},
		}, metav1.CreateOptions{})
	} else if err != nil {
		return err
	}

	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		for {
			select {
			case <-ticker.C:
				ctx := logs.NewContext()
				if err := k.refresh(ctx); err != nil {
					log.Error(err, "failed to refresh key store")
				}
			case <-k.Stop:
				klog.InfoS("api-key refreshing stopped")
				return
			}
		}
	}()
	return k.refresh(ctx)
}

func (k *SecretKeyStorage) refresh(ctx context.Context) error {
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

func (k *SecretKeyStorage) LoadByKey(key string) (*models.CreatedTeamAPIKey, bool) {
	value, ok := k.idxByKey.Load(key)
	if !ok {
		return nil, false
	}
	return value.(*models.CreatedTeamAPIKey), true
}

func (k *SecretKeyStorage) LoadByID(id string) (*models.CreatedTeamAPIKey, bool) {
	value, ok := k.idxByID.Load(id)
	if !ok {
		return nil, false
	}
	return value.(*models.CreatedTeamAPIKey), true
}

func (k *SecretKeyStorage) updateSecret(ctx context.Context, id string, apiKey *models.CreatedTeamAPIKey) error {
	secret, err := k.Client.CoreV1().Secrets(k.Namespace).Get(ctx, KeySecretName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	secret = secret.DeepCopy()
	if apiKey != nil {
		marshaled, err := json.Marshal(apiKey)
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

func (k *SecretKeyStorage) storeKey(apiKey *models.CreatedTeamAPIKey) {
	k.idxByKey.Store(apiKey.Key, apiKey)
	k.idxByID.Store(apiKey.ID.String(), apiKey)
}

func (k *SecretKeyStorage) CreateKey(ctx context.Context, user *models.CreatedTeamAPIKey, name string) (*models.CreatedTeamAPIKey, error) {
	log := klog.FromContext(ctx).WithValues("name", name).V(consts.DebugLogLevel)
	if name == "" || user == nil {
		return nil, errors.New("api-key name and user are required")
	}

	var newID, newKey uuid.UUID
	for i := 0; i < 100; i++ {
		newID = uuid.New()
		newKey = uuid.New()
		_, ok1 := k.LoadByID(newID.String())
		_, ok2 := k.LoadByKey(newKey.String())
		if !ok1 && !ok2 {
			break
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
	if err := k.updateSecret(ctx, newID.String(), apiKey); err != nil {
		log.Error(err, "failed to update api-key")
		return nil, err
	}
	k.storeKey(apiKey)
	return apiKey, nil
}

func (k *SecretKeyStorage) DeleteKey(ctx context.Context, key *models.CreatedTeamAPIKey) error {
	if key == nil {
		return nil
	}
	err := k.updateSecret(ctx, key.ID.String(), nil)
	if err != nil {
		return err
	}
	k.idxByKey.Delete(key.Key)
	k.idxByID.Delete(key.ID.String())
	return nil
}

func (k *SecretKeyStorage) ListByOwner(owner uuid.UUID) []*models.TeamAPIKey {
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
	return result
}
