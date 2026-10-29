package predict

import "database/sql"

// Cache is the judgment cache over the judgments table.
type Cache struct{ db *sql.DB }

// NewCache returns a cache over db, which must have the store migrations
// applied.
func NewCache(db *sql.DB) *Cache { return &Cache{db: db} }
