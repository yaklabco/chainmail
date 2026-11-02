package watch

import "database/sql"

// SetCommit replaces how e commits a batch's transaction, so a test can fail
// a commit the way a crash or a full disk would. Pass (*sql.Tx).Commit to
// restore it.
func SetCommit(e *Engine, commit func(*sql.Tx) error) { e.commitTx = commit }
