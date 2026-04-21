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

package keys

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jellydator/ttlcache/v3"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

// AdminTeamUID is the well-known UUID for the default admin team row.
var AdminTeamUID = uuid.MustParse("550e8400-e29b-41d4-a716-446655449999")

// errUUIDCollision is returned when a generated UUID conflicts with an existing key_hash or uid.
var errUUIDCollision = errors.New("uuid collision detected")

const (
	adminTeamName = "admin"
	keyCacheTTL   = 10 * time.Minute
)

var openMySQLDB = func(dsn string) (*gorm.DB, error) {
	return gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Warn),
		TranslateError: true,
	})
}

var autoMigrateMySQLModels = func(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).AutoMigrate(&TeamEntity{}, &TeamAPIKeyEntity{})
}

// TeamEntity corresponds to the teams table.
type TeamEntity struct {
	gorm.Model
	UID  string `gorm:"column:uid;type:varchar(36);uniqueIndex;not null"`
	Name string `gorm:"type:varchar(255);index;not null"`
}

// TableName overrides the table name.
func (TeamEntity) TableName() string { return "teams" }

// TeamAPIKeyEntity corresponds to the team_api_keys table.
// Key holds HMAC-SHA256(pepper, raw API key) as hex (64 chars), never plaintext.
type TeamAPIKeyEntity struct {
	gorm.Model
	UID          string  `gorm:"column:uid;type:varchar(36);uniqueIndex;not null"`
	Name         string  `gorm:"type:varchar(255);not null"`
	Key          string  `gorm:"column:key_hash;type:char(64);uniqueIndex;not null"`
	TeamID       uint    `gorm:"not null;index"`
	CreatedByUID *string `gorm:"column:created_by_uid;type:varchar(36);index"`
}

// TableName overrides the table name.
func (TeamAPIKeyEntity) TableName() string { return "team_api_keys" }

type mysqlConfig struct {
	DSN                string
	AdminKey           string
	Pepper             string
	DisableAutoMigrate bool
}

// mysqlKeyStorage implements KeyStorage using MySQL and GORM.
type mysqlKeyStorage struct {
	cfg mysqlConfig
	db  *gorm.DB

	byKey *ttlcache.Cache[string, *models.CreatedTeamAPIKey] // key: HMAC hex
	byID  *ttlcache.Cache[string, *models.CreatedTeamAPIKey] // key: API key uid

	stop     chan struct{}
	stopOnce sync.Once
}

func newMySQLKeyStorage(cfg mysqlConfig) *mysqlKeyStorage {
	return &mysqlKeyStorage{
		cfg: cfg,
		byKey: ttlcache.New[string, *models.CreatedTeamAPIKey](
			ttlcache.WithTTL[string, *models.CreatedTeamAPIKey](keyCacheTTL),
		),
		byID: ttlcache.New[string, *models.CreatedTeamAPIKey](
			ttlcache.WithTTL[string, *models.CreatedTeamAPIKey](keyCacheTTL),
		),
		stop: make(chan struct{}),
	}
}

// Init connects to MySQL and ensures admin team and admin API key rows exist.
// Schema auto-migration is skipped when explicitly disabled by config.
func (k *mysqlKeyStorage) Init(ctx context.Context) error {
	log := klog.FromContext(ctx)
	log.Info("initializing mysql key storage")

	db, err := openMySQLDB(k.cfg.DSN)
	if err != nil {
		return fmt.Errorf("open mysql: %w", err)
	}
	k.db = db

	if k.cfg.DisableAutoMigrate {
		log.Info("skip mysql schema auto-migration for key storage because disable-auto-migrate is enabled")
	} else if err := autoMigrateMySQLModels(ctx, k.db); err != nil {
		return fmt.Errorf("automigrate: %w", err)
	}

	adminTeamID, err := k.ensureAdminTeam(ctx)
	if err != nil {
		return err
	}
	return k.ensureAdminKey(ctx, adminTeamID)
}

func (k *mysqlKeyStorage) ensureAdminTeam(ctx context.Context) (uint, error) {
	team := TeamEntity{UID: AdminTeamUID.String(), Name: adminTeamName}
	if err := k.db.WithContext(ctx).Where(&TeamEntity{UID: team.UID}).
		Attrs(&TeamEntity{Name: adminTeamName}).
		FirstOrCreate(&team).Error; err != nil {
		return 0, fmt.Errorf("ensure admin team: %w", err)
	}
	return team.ID, nil
}

func (k *mysqlKeyStorage) ensureAdminKey(ctx context.Context, adminTeamID uint) error {
	hash := k.hashKey(k.cfg.AdminKey)
	var existing TeamAPIKeyEntity
	err := k.db.WithContext(ctx).Unscoped().Where(&TeamAPIKeyEntity{UID: AdminKeyID.String()}).First(&existing).Error
	if err == nil {
		updates := map[string]any{
			"name":     adminTeamName,
			"key_hash": hash,
			"team_id":  adminTeamID,
		}
		if err := k.db.WithContext(ctx).Model(&TeamAPIKeyEntity{}).
			Where("uid = ?", AdminKeyID.String()).
			Updates(updates).Error; err != nil {
			return fmt.Errorf("update admin key: %w", err)
		}
		if existing.DeletedAt.Valid {
			if err := k.db.WithContext(ctx).Unscoped().Model(&TeamAPIKeyEntity{}).
				Where("uid = ?", AdminKeyID.String()).
				Update("deleted_at", nil).Error; err != nil {
				return fmt.Errorf("restore admin key: %w", err)
			}
		}
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("lookup admin key by uid: %w", err)
	}
	entity := TeamAPIKeyEntity{
		UID:    AdminKeyID.String(),
		Name:   adminTeamName,
		Key:    hash,
		TeamID: adminTeamID,
	}
	if err := k.db.WithContext(ctx).Create(&entity).Error; err != nil {
		return fmt.Errorf("create admin key: %w", err)
	}
	return nil
}

// Run starts TTL cache janitors.
func (k *mysqlKeyStorage) Run() {
	go k.byKey.Start()
	go k.byID.Start()
	go func() {
		<-k.stop
		k.byKey.Stop()
		k.byID.Stop()
	}()
}

// Stop signals janitor goroutines to exit and releases resources.
func (k *mysqlKeyStorage) Stop() {
	k.stopOnce.Do(func() {
		close(k.stop)
	})
}

// LoadByKey resolves an API key by raw value (HMAC + cache + DB).
func (k *mysqlKeyStorage) LoadByKey(ctx context.Context, key string) (*models.CreatedTeamAPIKey, bool) {
	hash := k.hashKey(key)
	if item := k.byKey.Get(hash); item != nil {
		return item.Value(), true
	}
	var e TeamAPIKeyEntity
	if err := k.db.WithContext(ctx).Where(&TeamAPIKeyEntity{Key: hash}).First(&e).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			klog.FromContext(ctx).Error(err, "load key by hash")
		}
		return nil, false
	}
	apiKey, err := k.entityToCreated(&e)
	if err != nil {
		klog.FromContext(ctx).Error(err, "parse key entity")
		return nil, false
	}
	k.cachePut(apiKey, hash)
	return apiKey, true
}

// LoadByID resolves an API key by uid (cache + DB).
func (k *mysqlKeyStorage) LoadByID(ctx context.Context, id string) (*models.CreatedTeamAPIKey, bool) {
	if item := k.byID.Get(id); item != nil {
		return item.Value(), true
	}
	var e TeamAPIKeyEntity
	if err := k.db.WithContext(ctx).Where(&TeamAPIKeyEntity{UID: id}).First(&e).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			klog.FromContext(ctx).Error(err, "load key by uid")
		}
		return nil, false
	}
	apiKey, err := k.entityToCreated(&e)
	if err != nil {
		klog.FromContext(ctx).Error(err, "parse key entity")
		return nil, false
	}
	k.cachePut(apiKey, e.Key)
	return apiKey, true
}

func (k *mysqlKeyStorage) cachePut(apiKey *models.CreatedTeamAPIKey, keyHash string) {
	k.byKey.Set(keyHash, apiKey, ttlcache.DefaultTTL)
	k.byID.Set(apiKey.ID.String(), apiKey, ttlcache.DefaultTTL)
}

// CreateKey generates a new API key, stores only the HMAC hash, and returns the raw key once.
func (k *mysqlKeyStorage) CreateKey(ctx context.Context, user *models.CreatedTeamAPIKey, name string) (*models.CreatedTeamAPIKey, error) {
	log := klog.FromContext(ctx).WithValues("name", name).V(consts.DebugLogLevel)
	if name == "" || user == nil {
		return nil, errors.New("api-key name and user are required")
	}
	teamID, err := k.teamIDByUID(ctx, AdminTeamUID.String())
	if err != nil {
		return nil, err
	}

	var newID, newKey uuid.UUID
	var entity TeamAPIKeyEntity
	if retryErr := retry.OnError(wait.Backoff{
		Steps:    5,
		Duration: 0,
	}, func(err error) bool {
		return errors.Is(err, errUUIDCollision)
	}, func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		newID = generateUUID()
		newKey = generateUUID()
		createdBy := user.ID.String()
		entity = TeamAPIKeyEntity{
			UID:          newID.String(),
			Name:         name,
			Key:          k.hashKey(newKey.String()),
			TeamID:       teamID,
			CreatedByUID: &createdBy,
		}
		if err := k.db.WithContext(ctx).Create(&entity).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return errUUIDCollision
			}
			return fmt.Errorf("insert api-key: %w", err)
		}
		return nil
	}); retryErr != nil {
		if errors.Is(retryErr, errUUIDCollision) {
			return nil, errors.New("failed to generate unique api-key")
		}
		return nil, retryErr
	}

	apiKey := &models.CreatedTeamAPIKey{
		CreatedAt: entity.CreatedAt,
		ID:        newID,
		Key:       newKey.String(),
		Name:      name,
		Mask:      models.IdentifierMaskingDetails{},
		CreatedBy: &models.TeamUser{ID: user.ID},
	}
	log.Info("api-key generated", "id", apiKey.ID)
	k.cachePut(apiKey, entity.Key)
	return apiKey, nil
}

// DeleteKey removes an API key from the database and caches.
func (k *mysqlKeyStorage) DeleteKey(ctx context.Context, key *models.CreatedTeamAPIKey) error {
	if key == nil {
		return nil
	}
	var hash string
	if key.Key != "" {
		hash = k.hashKey(key.Key)
	} else {
		var e TeamAPIKeyEntity
		if err := k.db.WithContext(ctx).Where(&TeamAPIKeyEntity{UID: key.ID.String()}).
			Select("key_hash").First(&e).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("prefetch key hash: %w", err)
			}
		} else {
			hash = e.Key
		}
	}
	if err := k.db.WithContext(ctx).Where(&TeamAPIKeyEntity{UID: key.ID.String()}).
		Delete(&TeamAPIKeyEntity{}).Error; err != nil {
		return fmt.Errorf("delete api-key: %w", err)
	}
	if hash != "" {
		k.byKey.Delete(hash)
	}
	k.byID.Delete(key.ID.String())
	return nil
}

const defaultListTimeout = 5 * time.Second

// ListByOwner lists API keys for the same team as the owner key (by TeamID), without using cache.
func (k *mysqlKeyStorage) ListByOwner(ctx context.Context, owner uuid.UUID) ([]*models.TeamAPIKey, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultListTimeout)
		defer cancel()
	}
	var ownerEntity TeamAPIKeyEntity
	if err := k.db.WithContext(ctx).Where(&TeamAPIKeyEntity{UID: owner.String()}).
		First(&ownerEntity).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup owner key: %w", err)
	}
	var entities []TeamAPIKeyEntity
	if err := k.db.WithContext(ctx).Where(&TeamAPIKeyEntity{TeamID: ownerEntity.TeamID}).
		Find(&entities).Error; err != nil {
		return nil, fmt.Errorf("list keys by team: %w", err)
	}
	out := make([]*models.TeamAPIKey, 0, len(entities))
	for i := range entities {
		e := &entities[i]
		id, err := uuid.Parse(e.UID)
		if err != nil {
			klog.FromContext(ctx).Error(err, "invalid uid in key entity", "uid", e.UID)
			continue
		}
		tk := &models.TeamAPIKey{
			CreatedAt: e.CreatedAt,
			ID:        id,
			Name:      e.Name,
			Mask:      models.IdentifierMaskingDetails{},
		}
		out = append(out, tk)
	}
	return out, nil
}

func (k *mysqlKeyStorage) hashKey(raw string) string {
	h := hmac.New(sha256.New, []byte(k.cfg.Pepper))
	_, _ = h.Write([]byte(raw))
	return hex.EncodeToString(h.Sum(nil))
}

func (k *mysqlKeyStorage) teamIDByUID(ctx context.Context, uid string) (uint, error) {
	var team TeamEntity
	if err := k.db.WithContext(ctx).Where(&TeamEntity{UID: uid}).First(&team).Error; err != nil {
		return 0, fmt.Errorf("lookup team by uid: %w", err)
	}
	return team.ID, nil
}

func (k *mysqlKeyStorage) entityToCreated(e *TeamAPIKeyEntity) (*models.CreatedTeamAPIKey, error) {
	id, err := uuid.Parse(e.UID)
	if err != nil {
		return nil, fmt.Errorf("parse entity uid %q: %w", e.UID, err)
	}
	var createdBy *models.TeamUser
	if e.CreatedByUID != nil && *e.CreatedByUID != "" {
		cbID, err := uuid.Parse(*e.CreatedByUID)
		if err != nil {
			return nil, fmt.Errorf("parse created_by_uid %q: %w", *e.CreatedByUID, err)
		}
		createdBy = &models.TeamUser{ID: cbID}
	}
	return &models.CreatedTeamAPIKey{
		CreatedAt: e.CreatedAt,
		ID:        id,
		Name:      e.Name,
		Mask:      models.IdentifierMaskingDetails{},
		CreatedBy: createdBy,
	}, nil
}
