package entity

import (
	"time"

	"github.com/google/uuid"
)

type PaginationOffset struct {
	Searcher     string   `json:"searcher"`
	SortingAfter []string `json:"sorting_after"`
}

type SearchParams struct {
	Query      string
	Type       *string
	Pagination *PaginationOffset
}

type SearchQueryResult struct {
	Results    []MemeSearchResult
	Pagination *PaginationOffset
}

type Choice struct {
}

type MemeCreateResult struct {
	Id              uuid.UUID
	Text            string
	Caption         string
	DuplicateStatus string
	// MediaUrl is a presigned URL of the stored original media in S3.
	MediaUrl string
}

type YouTubeJobStatus struct {
	State    string // "pending", "running", "done", "failed"
	S3Path   string
	MimeType string
	Error    string
}

const ResultTypeImage = "image"
const ResultTypeVideo = "video"

type MemeSearchResult struct {
	Id string

	MediaUrl    string
	MediaWidth  int
	MediaHeight int

	ThumbUrl    string
	ThumbWidth  int
	ThumbHeight int

	Type string

	Caption string
}

type MediaFileCache struct {
	MemeId uuid.UUID `gorm:"type:uuid;primaryKey"`
	// Type is ResultTypeImage or ResultTypeVideo; it decides which cached
	// inline result the file_id is sent as.
	Type         string `gorm:"not null"`
	FileId       string `gorm:"not null"`
	FileUniqueId string
	Size         int64
	CreatedAt    time.Time
}

func (MediaFileCache) TableName() string { return "media_file_cache" }

type MtprotoSession struct {
	Id        int16 `gorm:"primaryKey"`
	Data      []byte
	UpdatedAt time.Time
}

func (MtprotoSession) TableName() string { return "mtproto_session" }
