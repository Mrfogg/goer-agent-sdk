// Package memorykit is a ready-made implementation of the SDK's memory
// contract: it gives an agent a durable, per-user memory store, the tools to
// write, fold and delete it, and a prompt block listing what is already known.
//
// Register it like any other module:
//
//	module, err := memorykit.New("memories.db", userID,
//		memorykit.WithMerger(memorykit.NewOpenAIMerger(client, model)),
//	)
//	if err != nil {
//		return err
//	}
//	agent.WithMemory(module)
//
// Storage is a gorm handle: New opens a SQLite file, NewWithDB takes a handle
// the host already owns. Only user-level memories are kept — durable facts and
// preferences about one user. A module is bound to a single user, so an
// application serving many users builds one module per user.
package memorykit

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"

	base "github.com/Mrfogg/goer-agent-sdk"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

const (
	// defaultCapacityHint is the number of stored memories at which the prompt
	// starts asking the model to fold overlapping memories together.
	defaultCapacityHint = 100
	// memoryTableName is the table the module owns.
	memoryTableName = "agent_memories"
)

// errMemoryNotFound is returned when an id does not resolve to a memory owned
// by the module's user. It covers both "never existed" and "not yours", so the
// error message never leaks another user's ids.
var errMemoryNotFound = errors.New("memory not found")

// memoryIDEncoding is lowercase base32hex (0-9a-v) without padding. Ten random
// bytes fill sixteen characters exactly, with no padding bits left over.
var memoryIDEncoding = base32.HexEncoding.WithPadding(base32.NoPadding)

// newID returns the id of a new memory: 10 random bytes, 16 characters of
// lowercase base32hex, 80 bits of entropy.
//
// It carries no timestamp, machine, or process information. Memory ids reach
// the model as [#id] markers, so they stay opaque instead.
func newID() string {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand does not fail on supported platforms; fall back to a
		// fixed-length placeholder rather than lose the memory.
		return "0000000000000000"
	}
	return strings.ToLower(memoryIDEncoding.EncodeToString(raw[:]))
}

// Memory is a single long-term memory of one user.
type Memory struct {
	ID      string `gorm:"primaryKey;size:64" json:"id"`
	UID     uint   `gorm:"index;not null" json:"uid"`
	Content string `gorm:"type:text;not null" json:"content"`
	// CreatedAt is written by gorm on insert and ordered on everywhere the
	// memories are listed, so the store reads back in the order it was built.
	CreatedAt int64 `gorm:"autoCreateTime" json:"created_at"`
}

func (Memory) TableName() string {
	return memoryTableName
}

// Module is the memory feature injected into an agent via WithMemory. It owns
// the user its memories belong to and provides both the memory tools and the
// prompt enrichment.
type Module struct {
	db       *gorm.DB
	uid      uint
	merger   Merger
	capacity int
}

var _ base.MemoryModule = (*Module)(nil)

// Option configures a Module.
type Option func(*Module)

// WithMerger enables memory_merge. Without a merger the tool is not registered
// and the prompt never mentions it, so the model is never offered a way to fold
// memories that does not work.
func WithMerger(merger Merger) Option {
	return func(m *Module) {
		m.merger = merger
	}
}

// WithCapacityHint overrides the number of stored memories at which the prompt
// starts asking for compression. Non-positive values are ignored.
func WithCapacityHint(count int) Option {
	return func(m *Module) {
		if count > 0 {
			m.capacity = count
		}
	}
}

// New opens the SQLite database at path, creates the memory table, and returns
// a module bound to one user. Use ":memory:" for a throwaway store.
func New(path string, uid uint, opts ...Option) (*Module, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("memory database path is required")
	}

	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("open memory database %q: %w", path, err)
	}
	if err := Migrate(db); err != nil {
		return nil, err
	}
	return NewWithDB(db, uid, opts...), nil
}

// NewWithDB builds a module on a gorm handle the host already owns, so the
// memories can live in the application's own database. The schema is not
// migrated here: call Migrate yourself, or use New.
func NewWithDB(db *gorm.DB, uid uint, opts ...Option) *Module {
	module := &Module{db: db, uid: uid, capacity: defaultCapacityHint}
	for _, opt := range opts {
		if opt != nil {
			opt(module)
		}
	}
	return module
}

// Migrate creates or updates the memory table on db.
func Migrate(db *gorm.DB) error {
	if db == nil {
		return errors.New("memory database is required")
	}
	if err := db.AutoMigrate(&Memory{}); err != nil {
		return fmt.Errorf("migrate memory table: %w", err)
	}
	return nil
}

func (m *Module) dbHandle() (*gorm.DB, error) {
	if m == nil || m.db == nil {
		return nil, errors.New("memory database is not initialized")
	}
	return m.db, nil
}

func (m *Module) requireUID() error {
	if m == nil || m.uid == 0 {
		return errors.New("memory uid is required")
	}
	return nil
}

// Save writes a memory: it inserts when memoryID is empty, and rewrites that
// record when it names one. The previous content comes back only when it
// actually changed, so the caller can say what the rewrite replaced.
func (m *Module) Save(ctx context.Context, content, memoryID string) (Memory, *Memory, error) {
	db, err := m.dbHandle()
	if err != nil {
		return Memory{}, nil, err
	}
	if err := m.requireUID(); err != nil {
		return Memory{}, nil, err
	}

	content = strings.TrimSpace(content)
	memoryID = strings.TrimSpace(memoryID)
	if content == "" {
		return Memory{}, nil, errors.New("memory content is required")
	}

	if memoryID == "" {
		record := Memory{ID: newID(), UID: m.uid, Content: content}
		if err := db.WithContext(ctx).Create(&record).Error; err != nil {
			return Memory{}, nil, err
		}
		return record, nil, nil
	}

	var target Memory
	if err := db.WithContext(ctx).
		Where("id = ? AND uid = ?", memoryID, m.uid).
		First(&target).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return Memory{}, nil, errMemoryNotFound
		}
		return Memory{}, nil, err
	}

	if err := db.WithContext(ctx).
		Model(&Memory{}).
		Where("id = ? AND uid = ?", memoryID, m.uid).
		Update("content", content).Error; err != nil {
		return Memory{}, nil, err
	}

	var record Memory
	if err := db.WithContext(ctx).
		Where("id = ? AND uid = ?", memoryID, m.uid).
		First(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return Memory{}, nil, errMemoryNotFound
		}
		return Memory{}, nil, err
	}
	if record.Content == target.Content {
		// Nothing was replaced, so there is nothing to report.
		return record, nil, nil
	}
	return record, &target, nil
}

// List returns every memory of the module's user, oldest first. The whole set
// is meant to fit in the prompt, so there is no pagination.
func (m *Module) List(ctx context.Context) ([]Memory, error) {
	db, err := m.dbHandle()
	if err != nil {
		return nil, err
	}
	if err := m.requireUID(); err != nil {
		return nil, err
	}

	var records []Memory
	if err := db.WithContext(ctx).
		Where("uid = ?", m.uid).
		Order("created_at ASC, id ASC").
		Find(&records).Error; err != nil {
		return nil, err
	}
	return records, nil
}

// Forget physically deletes one memory. It returns the deleted record so the
// caller can echo what disappeared. This is the only deletion path: there is no
// soft delete, no status field, and nothing to roll back.
func (m *Module) Forget(ctx context.Context, id string) (Memory, error) {
	db, err := m.dbHandle()
	if err != nil {
		return Memory{}, err
	}
	if err := m.requireUID(); err != nil {
		return Memory{}, err
	}

	id = strings.TrimSpace(id)
	if id == "" {
		return Memory{}, errors.New("memory id is required")
	}

	var record Memory
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND uid = ?", id, m.uid).First(&record).Error; err != nil {
			return err
		}
		result := tx.Where("id = ? AND uid = ?", id, m.uid).Delete(&Memory{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errMemoryNotFound
		}
		return nil
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Memory{}, errMemoryNotFound
	}
	if err != nil {
		return Memory{}, err
	}
	return record, nil
}

// load resolves memory ids that all belong to the module's user, in a stable
// order. A short result means at least one id was stale or owned by somebody
// else, which is an error rather than something to silently drop.
func (m *Module) load(ctx context.Context, ids []string) ([]Memory, error) {
	db, err := m.dbHandle()
	if err != nil {
		return nil, err
	}
	if err := m.requireUID(); err != nil {
		return nil, err
	}

	unique := uniqueIDs(ids)
	if len(unique) < mergeMinSources {
		return nil, fmt.Errorf("at least %d distinct memory ids are required", mergeMinSources)
	}

	var records []Memory
	if err := db.WithContext(ctx).
		Where("id IN ? AND uid = ?", unique, m.uid).
		Order("created_at ASC, id ASC").
		Find(&records).Error; err != nil {
		return nil, err
	}
	if len(records) != len(unique) {
		return nil, fmt.Errorf(
			"could not load %d of %d memories; they may have been deleted or belong to another user",
			len(unique)-len(records), len(unique),
		)
	}
	return records, nil
}

// replaceWith folds the source records into one new record inside a single
// transaction: either every source is deleted and the merged record exists, or
// nothing changed at all.
func (m *Module) replaceWith(ctx context.Context, sources []Memory, content string) (Memory, error) {
	db, err := m.dbHandle()
	if err != nil {
		return Memory{}, err
	}
	if err := m.requireUID(); err != nil {
		return Memory{}, err
	}

	content = strings.TrimSpace(content)
	if content == "" {
		return Memory{}, errors.New("memory content is required")
	}
	if len(sources) == 0 {
		return Memory{}, errors.New("memory sources are required")
	}

	ids := make([]string, 0, len(sources))
	for _, source := range sources {
		ids = append(ids, source.ID)
	}

	record := Memory{ID: newID(), UID: m.uid, Content: content}
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Where("id IN ? AND uid = ?", ids, m.uid).Delete(&Memory{})
		if result.Error != nil {
			return result.Error
		}
		if int(result.RowsAffected) != len(ids) {
			return fmt.Errorf(
				"%d of %d source memories changed while merging; nothing was merged",
				len(ids)-int(result.RowsAffected), len(ids),
			)
		}
		return tx.Create(&record).Error
	})
	if err != nil {
		return Memory{}, err
	}
	return record, nil
}

// uniqueIDs trims ids, drops blanks and collapses duplicates while keeping the
// caller's order.
func uniqueIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return unique
}
