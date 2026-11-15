package store

import "context"

// RunFilesGrantUploader consumes the originally selected directory. Callers
// retain ownership and close the grant only after all users have finished.
type RunFilesGrantUploader interface {
	UploadRunFilesFrom(context.Context, string, RunFilesGrant) (int, error)
}

func AsRunFilesGrantUploader(s RunStore) RunFilesGrantUploader {
	return capability[RunFilesGrantUploader](s)
}
