package main

import "time"

type VideoKind string

const (
	VideoKindUnknown VideoKind = "unknown"
	VideoKindVideo   VideoKind = "video"
	VideoKindShort   VideoKind = "short"
)

type ChannelRecord struct {
	ID                string
	Title             string
	UploadsPlaylistID string
	WebSubSecret      string
	LeaseStartedAt    int64
	LeaseExpiresAt    int64
	LastReconciledAt  int64
}

type VideoRecord struct {
	ID           string
	ChannelID    string
	ChannelTitle string
	Title        string
	PublishedAt  int64
	UpdatedAt    int64
	Kind         VideoKind
}

type ChannelMetadata struct {
	ID                string
	Title             string
	UploadsPlaylistID string
}

type PlaylistItem struct {
	VideoID     string
	Title       string
	AddedAt     time.Time
	PublishedAt time.Time
}

type PlaylistPage struct {
	Items         []PlaylistItem
	NextPageToken string
}

type PendingVideo struct {
	ID          string
	PublishedAt int64
}

type VideoKindUpdate struct {
	VideoID string
	Kind    VideoKind
}
