//go:build ignore

package db

// db.go is superseded by open.go + models.go + user_store.go + errors.go.
// All content moved to:
//   - open.go: Open() + pragmas + config keys
//   - models.go: tables
//   - user_store.go: UserBy* / GetOrCreateUser / IsSuperAdmin
//   - errors.go: shared errors
