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
	"golang.org/x/sync/singleflight"
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
var AdminTeamUID = models.AdminTeamID

// errRetryableDuplicateKey is returned when a create path hits a duplicate-key
// conflict that should be retried, such as concurrent team creation or a
// generated key/uid collision.
var errRetryableDuplicateKey = errors.New("retryable duplicate key conflict")

const (
	adminTeamName = models.AdminTeamName
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
	// UID is display-only compatibility metadata. Team identity and authorization use Name.
	UID  string `gorm:"column:uid;type:varchar(36);uniqueIndex;not null"`
	Name string `gorm:"type:varchar(255);uniqueIndex;not null"`
}

// TableName overrides the table name.
func (TeamEntity) TableName() string { return "teams" }

// TeamAPIKeyEntity corresponds to the team_api_keys table.
// KeyHash holds HMAC-SHA256(pepper, raw API key) as hex (64 chars), never plaintext.
type TeamAPIKeyEntity struct {
	gorm.Model
	UID     string `gorm:"column:uid;type:varchar(36);uniqueIndex;not null"`
	Name    string `gorm:"type:varchar(255);not null"`
	KeyHash string `gorm:"column:key_hash;type:char(64);uniqueIndex;not null"`
	TeamID  uint   `gorm:"not null;index"`
	// CreatedByUID is creator metadata only. Do not use it for ownership or authorization.
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

	teamCache *ttlcache.Cache[string, *models.Team]
	teamGroup singleflight.Group

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
		teamCache: ttlcache.New[string, *models.Team](
			ttlcache.WithTTL[string, *models.Team](10 * time.Minute),
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
	if err := k.db.WithContext(ctx).Unscoped().Where(&TeamEntity{Name: adminTeamName}).
		Attrs(&TeamEntity{Name: adminTeamName}).
		FirstOrCreate(&team).Error; err != nil {
		return 0, fmt.Errorf("ensure admin team: %w", err)
	}
	if team.UID != AdminTeamUID.String() {
		if err := k.db.WithContext(ctx).Unscoped().Model(&TeamEntity{}).
			Where("id = ?", team.ID).
			Update("uid", AdminTeamUID.String()).Error; err != nil {
			return 0, fmt.Errorf("update admin team uid: %w", err)
		}
	}

	return team.ID, nil
}

func (k *mysqlKeyStorage) ensureAdminKey(ctx context.Context, adminTeamID uint) error {
	hash := k.hashKey(k.cfg.AdminKey)
	key := TeamAPIKeyEntity{
		UID:     AdminKeyID.String(),
		Name:    adminTeamName,
		KeyHash: hash,
		TeamID:  adminTeamID,
	}

	var existing TeamAPIKeyEntity
	if err := k.db.WithContext(ctx).Unscoped().Where(&TeamAPIKeyEntity{UID: key.UID}).
		First(&existing).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("lookup admin key by uid: %w", err)
		}
		if err := k.db.WithContext(ctx).Create(&key).Error; err != nil {
			return fmt.Errorf("create admin key: %w", err)
		}
		return nil
	}

	if err := k.db.WithContext(ctx).Model(&TeamAPIKeyEntity{}).
		Where("uid = ?", key.UID).
		Updates(map[string]any{
			"name":     key.Name,
			"key_hash": key.KeyHash,
			"team_id":  key.TeamID,
		}).Error; err != nil {
		return fmt.Errorf("update admin key: %w", err)
	}

	return nil
}

// Run starts TTL cache janitors.
func (k *mysqlKeyStorage) Run() {
	go k.byKey.Start()
	go k.byID.Start()
	go k.teamCache.Start()
	go func() {
		<-k.stop
		k.byKey.Stop()
		k.byID.Stop()
		k.teamCache.Stop()
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
	apiKey, err := k.loadCreatedKeyFromDB(ctx, &TeamAPIKeyEntity{KeyHash: hash})
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			klog.FromContext(ctx).Error(err, "load key by hash")
		}
		return nil, false
	}
	k.cachePut(apiKey)
	return apiKey, true
}

// LoadByID resolves an API key by uid (cache + DB).
func (k *mysqlKeyStorage) LoadByID(ctx context.Context, id string) (*models.CreatedTeamAPIKey, bool) {
	if item := k.byID.Get(id); item != nil {
		return item.Value(), true
	}
	apiKey, err := k.loadCreatedKeyFromDB(ctx, &TeamAPIKeyEntity{UID: id})
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			klog.FromContext(ctx).Error(err, "load key by uid")
		}
		return nil, false
	}
	k.cachePut(apiKey)
	return apiKey, true
}

func (k *mysqlKeyStorage) cachePut(apiKey *models.CreatedTeamAPIKey) {
	if apiKey.KeyHash != "" {
		k.byKey.Set(apiKey.KeyHash, apiKey, ttlcache.DefaultTTL)
	}
	k.byID.Set(apiKey.ID.String(), apiKey, ttlcache.DefaultTTL)
}

// CreateKey generates a new API key, stores only the HMAC hash, and returns the raw key once.
func (k *mysqlKeyStorage) CreateKey(ctx context.Context, key *models.CreatedTeamAPIKey, opts CreateKeyOptions) (*models.CreatedTeamAPIKey, error) {
	log := klog.FromContext(ctx).WithValues("name", opts.Name).V(consts.DebugLogLevel)
	teamName, err := validateCreateKeyOptions(key, opts)
	if err != nil {
		return nil, err
	}

	var newID, newKey uuid.UUID
	var entity TeamAPIKeyEntity
	var team *models.Team
	if retryErr := retry.OnError(wait.Backoff{
		Steps:    5,
		Duration: 0,
	}, func(err error) bool {
		return errors.Is(err, errRetryableDuplicateKey)
	}, func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		newID = generateUUID()
		newKey = generateUUID()
		createdBy := key.ID.String()
		return k.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			teamEntity, err := k.resolveTeamByName(ctx, tx, teamName)
			if err != nil {
				return err
			}
			team, err = teamFromEntity(teamEntity)
			if err != nil {
				return err
			}
			entity = TeamAPIKeyEntity{
				UID:          newID.String(),
				Name:         opts.Name,
				KeyHash:      k.hashKey(newKey.String()),
				TeamID:       teamEntity.ID,
				CreatedByUID: &createdBy,
			}
			if err := tx.Create(&entity).Error; err != nil {
				if errors.Is(err, gorm.ErrDuplicatedKey) {
					return errRetryableDuplicateKey
				}
				return fmt.Errorf("insert api-key: %w", err)
			}
			return nil
		})
	}); retryErr != nil {
		if errors.Is(retryErr, errRetryableDuplicateKey) {
			return nil, errors.New("failed to generate unique api-key")
		}
		return nil, retryErr
	}

	apiKey := &models.CreatedTeamAPIKey{
		CreatedAt: entity.CreatedAt,
		ID:        newID,
		Key:       newKey.String(),
		KeyHash:   entity.KeyHash,
		Name:      opts.Name,
		Mask:      models.IdentifierMaskingDetails{},
		Team:      team,
		CreatedBy: &models.TeamUser{ID: key.ID},
	}
	log.Info("api-key generated", "id", apiKey.ID)
	k.cachePut(apiKey)
	return apiKey, nil
}

// DeleteKey removes an API key from the database and caches.
func (k *mysqlKeyStorage) DeleteKey(ctx context.Context, key *models.CreatedTeamAPIKey) error {
	log := klog.FromContext(ctx).V(consts.DebugLogLevel)
	if key == nil {
		return nil
	}
	if key.ID == AdminKeyID {
		return ErrAdminKeyUndeletable
	}
	hash := key.KeyHash
	err := k.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var entityToBeDeleted TeamAPIKeyEntity
		if err := tx.Where(&TeamAPIKeyEntity{UID: key.ID.String()}).First(&entityToBeDeleted).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return fmt.Errorf("load api-key before delete: %w", err)
		}
		hash = entityToBeDeleted.KeyHash
		var team TeamEntity
		if err := tx.Where(&TeamEntity{Model: gorm.Model{ID: entityToBeDeleted.TeamID}}).First(&team).Error; err != nil {
			return fmt.Errorf("lookup team before delete: %w", err)
		}
		if err := tx.Unscoped().Delete(&TeamAPIKeyEntity{}, entityToBeDeleted.ID).Error; err != nil {
			return fmt.Errorf("delete api-key: %w", err)
		}
		if team.Name != models.AdminTeamName {
			// If this was the last key of the team, delete the team too.
			var count int64
			if err := tx.Model(&TeamAPIKeyEntity{}).Where("team_id = ?", entityToBeDeleted.TeamID).Count(&count).Error; err != nil {
				return fmt.Errorf("count team api-keys: %w", err)
			}
			if count == 0 {
				if err := tx.Unscoped().Delete(&TeamEntity{}, entityToBeDeleted.TeamID).Error; err != nil {
					return fmt.Errorf("delete team: %w", err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	log.Info("api-key deleted from db", "id", key.ID, "hash", hash)
	if hash != "" {
		k.byKey.Delete(hash)
	}
	k.byID.Delete(key.ID.String())
	log.Info("api-key deleted from cache", "id", key.ID, "hash", hash)
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
			CreatedBy: teamUserFromUID(ctx, e.CreatedByUID),
		}
		out = append(out, tk)
	}
	return out, nil
}

func (k *mysqlKeyStorage) ListTeams(ctx context.Context, user *models.CreatedTeamAPIKey) ([]*models.ListedTeam, error) {
	if user == nil {
		return nil, nil
	}
	log := klog.FromContext(ctx)
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultListTimeout)
		defer cancel()
	}
	userTeam := TeamForKey(user)
	var entities []TeamEntity
	query := k.db.WithContext(ctx).Model(&TeamEntity{})
	if userTeam.Name != models.AdminTeamName {
		query = query.Where(&TeamEntity{Name: userTeam.Name})
	}
	if err := query.Find(&entities).Error; err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	result := make([]*models.ListedTeam, 0, len(entities))
	for i := range entities {
		team, err := teamFromEntity(&entities[i])
		if err != nil {
			log.Error(err, "failed to convert team entity to model, skip", "entity", entities[i])
			continue
		}
		result = append(result, listedTeam(team, entities[i].Name == userTeam.Name))
	}
	return result, nil
}

func (k *mysqlKeyStorage) FindTeamByName(ctx context.Context, teamName string) (*models.Team, bool, error) {
	if item := k.teamCache.Get(teamName); item != nil {
		team := item.Value()
		return cloneTeam(team), true, nil
	}

	result, err, _ := k.teamGroup.Do(teamName, func() (any, error) {
		var team TeamEntity
		if err := k.db.WithContext(ctx).Where(&TeamEntity{Name: teamName}).First(&team).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, nil
			}
			return nil, fmt.Errorf("lookup team by name: %w", err)
		}
		t, err := teamFromEntity(&team)
		if err != nil {
			return nil, err
		}
		k.teamCache.Set(teamName, t, ttlcache.DefaultTTL)
		return t, nil
	})

	if err != nil {
		return nil, false, err
	}
	if result == nil {
		return nil, false, nil
	}
	team := result.(*models.Team)
	return cloneTeam(team), true, nil
}

func (k *mysqlKeyStorage) hashKey(raw string) string {
	h := hmac.New(sha256.New, []byte(k.cfg.Pepper))
	_, _ = h.Write([]byte(raw))
	return hex.EncodeToString(h.Sum(nil))
}

func (k *mysqlKeyStorage) resolveTeamByName(ctx context.Context, tx *gorm.DB, teamName string) (*TeamEntity, error) {
	team := &TeamEntity{}
	if err := tx.WithContext(ctx).Unscoped().Where(&TeamEntity{Name: teamName}).First(team).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("lookup team by name: %w", err)
		}
		// Team does not exist, create it.
		team.Name = teamName
		if teamName == adminTeamName {
			team.UID = AdminTeamUID.String()
		} else {
			team.UID = generateUUID().String()
		}
		if err := tx.WithContext(ctx).Create(team).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return nil, errRetryableDuplicateKey
			}
			return nil, fmt.Errorf("create team: %w", err)
		}
		return team, nil
	}
	return team, nil
}

func teamUserFromUID(ctx context.Context, uid *string) *models.TeamUser {
	if uid == nil || *uid == "" {
		return nil
	}
	id, err := uuid.Parse(*uid)
	if err != nil {
		klog.FromContext(ctx).Error(err, "invalid created_by_uid in key entity", "created_by_uid", *uid)
		return nil
	}
	return &models.TeamUser{ID: id}
}

func teamFromEntity(entity *TeamEntity) (*models.Team, error) {
	id, err := uuid.Parse(entity.UID)
	if err != nil {
		return nil, fmt.Errorf("parse team uid %q: %w", entity.UID, err)
	}
	return &models.Team{ID: id, Name: entity.Name}, nil
}

func (k *mysqlKeyStorage) loadCreatedKeyFromDB(ctx context.Context, where *TeamAPIKeyEntity) (*models.CreatedTeamAPIKey, error) {
	var e TeamAPIKeyEntity
	if err := k.db.WithContext(ctx).Where(where).First(&e).Error; err != nil {
		return nil, err
	}
	id, err := uuid.Parse(e.UID)
	if err != nil {
		return nil, fmt.Errorf("parse entity uid %q: %w", e.UID, err)
	}
	createdBy := teamUserFromUID(ctx, e.CreatedByUID)

	var teamEntity TeamEntity
	if err := k.db.WithContext(ctx).Where(&TeamEntity{Model: gorm.Model{ID: e.TeamID}}).First(&teamEntity).Error; err != nil {
		return nil, fmt.Errorf("lookup team by id %d: %w", e.TeamID, err)
	}
	team, err := teamFromEntity(&teamEntity)
	if err != nil {
		return nil, err
	}

	return &models.CreatedTeamAPIKey{
		CreatedAt: e.CreatedAt,
		ID:        id,
		KeyHash:   e.KeyHash,
		Name:      e.Name,
		Mask:      models.IdentifierMaskingDetails{},
		CreatedBy: createdBy,
		Team:      team,
	}, nil
}
