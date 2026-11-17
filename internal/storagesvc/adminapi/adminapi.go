// Package adminapi holds the wire types of the storage service's admin
// endpoints (/internal/*, docs/openapi-storage.yaml), shared by the service
// and the hosted API's client.
package adminapi

import "time"

// MaxDeleteKeys is the most keys one DELETE /internal/blobs call takes, and
// one DELETE /internal/users/{userId} call deletes: S3's batch delete limit.
const MaxDeleteKeys = 1000

// DeleteBlobsResponse is the body of DELETE /internal/blobs.
type DeleteBlobsResponse struct {
	// Deleted counts the keys that are now absent, including ones that were
	// already gone.
	Deleted int          `json:"deleted"`
	Failed  []KeyFailure `json:"failed"`
}

// KeyFailure is a key DELETE /internal/blobs could not remove.
type KeyFailure struct {
	Key     string `json:"key"`
	Message string `json:"message"`
}

// ListBlobsResponse is the body of GET /internal/blobs.
type ListBlobsResponse struct {
	Blobs []Blob `json:"blobs"`
	// NextCursor resumes the listing; null once it is complete.
	NextCursor *string `json:"next_cursor"`
}

// Blob describes a stored blob.
type Blob struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
}

// DeleteUserResponse is the body of DELETE /internal/users/{userId}.
type DeleteUserResponse struct {
	Deleted int  `json:"deleted"`
	Done    bool `json:"done"`
}
