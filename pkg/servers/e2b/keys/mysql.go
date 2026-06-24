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

	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/utils"
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

type joinedAPIKeyRow struct {
	TeamAPIKeyEntity
	TeamUID  string `gorm:"column:team_uid"`
	TeamName string `gorm:"column:team_name"`
}

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
		if err := k.db.WithContext(ctx).Session(&gorm.Session{SkipDefaultTransaction: true}).Unscoped().Model(&TeamEntity{}).
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
		if err := k.db.WithContext(ctx).Session(&gorm.Session{SkipDefaultTransaction: true}).Create(&key).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return k.repairAdminKeyAfterDuplicate(ctx, key)
			}
			return fmt.Errorf("create admin key: %w", err)
		}
		return nil
	}

	return k.updateAdminKey(ctx, key)
}

func (k *mysqlKeyStorage) repairAdminKeyAfterDuplicate(ctx context.Context, key TeamAPIKeyEntity) error {
	var existing TeamAPIKeyEntity
	if err := k.db.WithContext(ctx).Unscoped().Where(&TeamAPIKeyEntity{UID: key.UID}).
		First(&existing).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("create admin key: duplicate conflict but admin uid %s not present", key.UID)
		}
		return fmt.Errorf("lookup admin key after duplicate insert: %w", err)
	}

	if adminKeyEqual(existing, key) {
		return nil
	}

	return k.updateAdminKey(ctx, key)
}

func adminKeyEqual(existing, expected TeamAPIKeyEntity) bool {
	return existing.UID == expected.UID &&
		existing.Name == expected.Name &&
		existing.KeyHash == expected.KeyHash &&
		existing.TeamID == expected.TeamID &&
		!existing.DeletedAt.Valid
}

func (k *mysqlKeyStorage) updateAdminKey(ctx context.Context, key TeamAPIKeyEntity) error {
	if err := k.db.WithContext(ctx).Session(&gorm.Session{SkipDefaultTransaction: true}).Model(&TeamAPIKeyEntity{}).
		Unscoped().
		Where("uid = ?", key.UID).
		Updates(map[string]any{
			"name":       key.Name,
			"key_hash":   key.KeyHash,
			"team_id":    key.TeamID,
			"deleted_at": nil,
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
	if apiKey := k.cacheGetByKey(hash); apiKey != nil {
		return apiKey, true
	}
	apiKey, err := k.loadCreatedKeyByHashFromDB(ctx, hash)
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			klog.FromContext(ctx).Error(err, "load key by hash")
		}
		return nil, false
	}
	k.cachePutKey(apiKey)
	return apiKey, true
}

// LoadByID resolves an API key by uid (cache + DB).
func (k *mysqlKeyStorage) LoadByID(ctx context.Context, id string) (*models.CreatedTeamAPIKey, bool) {
	if apiKey := k.cacheGetByID(id); apiKey != nil {
		return apiKey, true
	}
	apiKey, err := k.loadCreatedKeyByUIDFromDB(ctx, id)
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			klog.FromContext(ctx).Error(err, "load key by uid")
		}
		return nil, false
	}
	k.cachePutKey(apiKey)
	return apiKey, true
}

func (k *mysqlKeyStorage) cacheGetByKey(hash string) *models.CreatedTeamAPIKey {
	if item := k.byKey.Get(hash); item != nil {
		return cloneCreatedTeamAPIKey(item.Value())
	}
	return nil
}

func (k *mysqlKeyStorage) cacheGetByID(id string) *models.CreatedTeamAPIKey {
	if item := k.byID.Get(id); item != nil {
		return cloneCreatedTeamAPIKey(item.Value())
	}
	return nil
}

func (k *mysqlKeyStorage) cachePutKey(apiKey *models.CreatedTeamAPIKey) {
	cached := cloneCreatedTeamAPIKey(apiKey)
	if cached == nil {
		return
	}
	cached.Key = "" // never store the plaintext key even in cache
	if cached.KeyHash != "" {
		k.byKey.Set(cached.KeyHash, cached, ttlcache.DefaultTTL)
	}
	k.byID.Set(cached.ID.String(), cached, ttlcache.DefaultTTL)
}

func (k *mysqlKeyStorage) cachePutTeam(team *models.Team) {
	if team == nil || team.Name == "" {
		return
	}
	k.teamCache.Set(team.Name, cloneTeam(team), ttlcache.DefaultTTL)
}

// cloneCreatedTeamAPIKey returns a deep copy safe to hand to callers without
// risking cache mutation. When adding new pointer or slice fields to
// `models.CreatedTeamAPIKey`, extend this helper so they stay
// independent of the cached instance.
func cloneCreatedTeamAPIKey(apiKey *models.CreatedTeamAPIKey) *models.CreatedTeamAPIKey {
	if apiKey == nil {
		return nil
	}
	cloned := *apiKey
	cloned.Team = cloneTeam(apiKey.Team)
	cloned.CreatedBy = cloneTeamUser(apiKey.CreatedBy)
	if apiKey.LastUsed != nil {
		lastUsed := *apiKey.LastUsed
		cloned.LastUsed = &lastUsed
	}
	return &cloned
}

func cloneTeamUser(user *models.TeamUser) *models.TeamUser {
	if user == nil {
		return nil
	}
	return &models.TeamUser{
		Email: user.Email,
		ID:    user.ID,
	}
}

// CreateKey generates a new API key, stores only the HMAC hash, and returns the raw key once.
func (k *mysqlKeyStorage) CreateKey(ctx context.Context, key *models.CreatedTeamAPIKey, opts CreateKeyOptions) (*models.CreatedTeamAPIKey, error) {
	log := klog.FromContext(ctx).WithValues("name", opts.Name).V(utils.DebugLogLevel)
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
		db := k.db.WithContext(ctx)
		teamEntity, err := k.getOrCreateTeamDB(ctx, db, teamName)
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
		if err := db.Session(&gorm.Session{SkipDefaultTransaction: true}).Create(&entity).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return errRetryableDuplicateKey
			}
			return fmt.Errorf("insert api-key: %w", err)
		}
		return nil
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
	k.cachePutTeam(team)
	k.cachePutKey(apiKey)
	return apiKey, nil
}

// DeleteKey removes only an API key from the database and caches. Created teams are reserved even if they have no keys.
func (k *mysqlKeyStorage) DeleteKey(ctx context.Context, key *models.CreatedTeamAPIKey) error {
	log := klog.FromContext(ctx).V(utils.DebugLogLevel)
	if key == nil {
		return nil
	}
	if key.ID == AdminKeyID {
		return ErrAdminKeyUndeletable
	}
	hash := key.KeyHash
	if hash == "" {
		if key.Key != "" {
			hash = k.hashKey(key.Key)
		} else {
			var entity TeamAPIKeyEntity
			// No `Unscoped()` here: this lookup is for cache invalidation, and
			// invalidating the cache entry of a row that was already
			// soft-deleted on a different code path would be wrong (its hash
			// may belong to a stale row that no longer authenticates).
			if err := k.db.WithContext(ctx).Model(&TeamAPIKeyEntity{}).
				Select("key_hash").
				Where("uid = ?", key.ID.String()).
				Take(&entity).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return nil
				}
				return fmt.Errorf("load api-key hash before delete: %w", err)
			}
			hash = entity.KeyHash
		}
	}

	// Skip the implicit transaction that GORM wraps around every write operation.
	// A single DELETE statement is already atomic, so the extra BEGIN/COMMIT
	// round-trip is unnecessary overhead.
	//
	// GORM's `Delete` returns `nil` (with `RowsAffected == 0`) when the target
	// row does not exist, so a missing key naturally falls through to the
	// "deleted" branch and the cache invalidation below — no special handling
	// needed.
	err := k.db.WithContext(ctx).Session(&gorm.Session{SkipDefaultTransaction: true}).
		Unscoped().
		Where("uid = ?", key.ID.String()).
		Delete(&TeamAPIKeyEntity{}).Error
	if err != nil {
		return fmt.Errorf("delete api-key: %w", err)
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

// ListByOwnerTeam lists API keys belonging to the same team as the owner, without
// using cache. The owner's team information is resolved from the passed-in
// CreatedTeamAPIKey (populated at authentication time), avoiding an extra DB
// round-trip.
func (k *mysqlKeyStorage) ListByOwnerTeam(ctx context.Context, owner *models.CreatedTeamAPIKey) ([]*models.TeamAPIKey, error) {
	if owner == nil {
		return nil, nil
	}
	teamName := TeamForKey(owner).Name

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultListTimeout)
		defer cancel()
	}
	var entities []TeamAPIKeyEntity
	// `teams.deleted_at IS NULL` keeps the subquery from picking up a soft-deleted
	// team that happens to share a name with an active one. Although the current
	// flow does not soft-delete teams, the predicate keeps this resolver
	// consistent with `loadCreatedKeyFromDB` and resilient to future changes.
	if err := k.db.WithContext(ctx).
		Model(&TeamAPIKeyEntity{}).
		Select("created_at", "uid", "name", "created_by_uid").
		Where("team_id = (SELECT id FROM teams WHERE name = ? AND deleted_at IS NULL LIMIT 1)", teamName).
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
	userTeam := TeamForKey(user)
	// Non-admin users can only see their own team.
	if userTeam.Name != models.AdminTeamName {
		return []*models.ListedTeam{listedTeam(userTeam, true)}, nil
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultListTimeout)
		defer cancel()
	}
	var entities []TeamEntity
	// Filter out teams with no API keys via an EXISTS subquery. The teams table
	// is the driving side; the subquery hits the indexed team_api_keys.team_id
	// column and short-circuits on the first matching row, so per-team overhead
	// stays small. The call is admin-only and low-frequency, and any runaway
	// scan is bounded by the `defaultListTimeout` deadline above.
	//
	// `team_api_keys.deleted_at IS NULL` excludes soft-deleted keys so a team
	// whose only keys have been deleted does not surface as "active".
	// GORM's auto soft-delete predicate on the outer `teams` query already
	// hides soft-deleted teams.
	if err := k.db.WithContext(ctx).Model(&TeamEntity{}).
		Select("uid", "name").
		Where("EXISTS (SELECT 1 FROM team_api_keys WHERE team_api_keys.team_id = teams.id AND team_api_keys.deleted_at IS NULL)").
		Find(&entities).Error; err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	result := make([]*models.ListedTeam, 0, len(entities))
	for i := range entities {
		team, err := teamFromEntity(&entities[i])
		if err != nil {
			log.Error(err, "failed to convert team entity to model, skip", "entity", entities[i])
			continue
		}
		k.cachePutTeam(team)
		result = append(result, listedTeam(team, entities[i].Name == userTeam.Name))
	}
	return result, nil
}

func (k *mysqlKeyStorage) FindTeamByName(ctx context.Context, teamName string) (*models.Team, bool, error) {
	if item := k.teamCache.Get(teamName); item != nil {
		team := item.Value()
		return cloneTeam(team), true, nil
	}

	var entity TeamEntity
	// GORM's `Where(&TeamEntity{...}).First` already filters
	// `deleted_at IS NULL`; soft-deleted teams must not satisfy a name lookup.
	if err := k.db.WithContext(ctx).Where(&TeamEntity{Name: teamName}).First(&entity).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("lookup team by name: %w", err)
	}
	team, err := teamFromEntity(&entity)
	if err != nil {
		return nil, false, err
	}
	k.cachePutTeam(team)
	return cloneTeam(team), true, nil
}

func (k *mysqlKeyStorage) hashKey(raw string) string {
	h := hmac.New(sha256.New, []byte(k.cfg.Pepper))
	_, _ = h.Write([]byte(raw))
	return hex.EncodeToString(h.Sum(nil))
}

func (k *mysqlKeyStorage) getOrCreateTeamDB(ctx context.Context, tx *gorm.DB, teamName string) (*TeamEntity, error) {
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
		if err := tx.WithContext(ctx).Session(&gorm.Session{SkipDefaultTransaction: true}).Create(team).Error; err != nil {
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

func (k *mysqlKeyStorage) loadCreatedKeyByHashFromDB(ctx context.Context, hash string) (*models.CreatedTeamAPIKey, error) {
	return k.loadCreatedKeyFromDB(ctx, func(query *gorm.DB) *gorm.DB {
		return query.Where("team_api_keys.key_hash = ?", hash)
	})
}

func (k *mysqlKeyStorage) loadCreatedKeyByUIDFromDB(ctx context.Context, uid string) (*models.CreatedTeamAPIKey, error) {
	return k.loadCreatedKeyFromDB(ctx, func(query *gorm.DB) *gorm.DB {
		return query.Where("team_api_keys.uid = ?", uid)
	})
}

func (k *mysqlKeyStorage) loadCreatedKeyFromDB(ctx context.Context, applyPredicate func(*gorm.DB) *gorm.DB) (*models.CreatedTeamAPIKey, error) {
	var row joinedAPIKeyRow
	// `Table("team_api_keys")` opts out of GORM's auto-managed soft-delete
	// predicate, so both `deleted_at IS NULL` clauses below must be added by
	// hand: the JOIN side hides soft-deleted teams (a key whose team has been
	// removed must not authenticate), and the outer WHERE hides soft-deleted
	// keys.
	query := k.db.WithContext(ctx).Table("team_api_keys").
		Select("team_api_keys.*, teams.uid AS team_uid, teams.name AS team_name").
		Joins("JOIN teams ON teams.id = team_api_keys.team_id AND teams.deleted_at IS NULL").
		Where("team_api_keys.deleted_at IS NULL")
	if applyPredicate != nil {
		query = applyPredicate(query)
	}
	if err := query.Take(&row).Error; err != nil {
		return nil, err
	}
	id, err := uuid.Parse(row.UID)
	if err != nil {
		return nil, fmt.Errorf("parse entity uid %q: %w", row.UID, err)
	}
	teamID, err := uuid.Parse(row.TeamUID)
	if err != nil {
		return nil, fmt.Errorf("parse team uid %q: %w", row.TeamUID, err)
	}
	createdBy := teamUserFromUID(ctx, row.CreatedByUID)
	team := &models.Team{
		ID:   teamID,
		Name: row.TeamName,
	}
	k.cachePutTeam(team)

	return &models.CreatedTeamAPIKey{
		CreatedAt: row.CreatedAt,
		ID:        id,
		KeyHash:   row.KeyHash,
		Name:      row.Name,
		Mask:      models.IdentifierMaskingDetails{},
		CreatedBy: createdBy,
		Team:      team,
	}, nil
}
