package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coalaura/schgo"
	_ "github.com/mattn/go-sqlite3"
)

const (
	DatabasePath = "data/database.db"

	listVideosQueryPrefix = `SELECT v.id, v.channel_id, c.title, v.title, v.published_at, v.updated_at, v.kind, v.member_only FROM videos v JOIN channels c ON c.id = v.channel_id WHERE v.channel_id IN (`
	listVideosKindFilter  = ` AND v.kind = ?`
	listVideosQuerySuffix = ` ORDER BY v.published_at DESC LIMIT ?`
)

type Database struct {
	db *sql.DB
}

func (d *Database) Close() error {
	return d.db.Close()
}

func (d *Database) EnsureChannel(ctx context.Context, channelID string) error {
	secret, err := randomSecret()
	if err != nil {
		return err
	}

	_, err = d.db.ExecContext(
		ctx,
		`INSERT INTO channels (id, websub_secret) VALUES (?, ?) ON CONFLICT(id) DO NOTHING`,
		channelID,
		secret,
	)

	if err != nil {
		return fmt.Errorf("ensure channel %s: %w", channelID, err)
	}

	return nil
}

func (d *Database) Channel(ctx context.Context, channelID string) (*ChannelRecord, error) {
	row := d.db.QueryRowContext(
		ctx,
		`SELECT
			id,
			title,
			uploads_playlist_id,
			websub_secret,
			COALESCE(lease_started_at, 0),
			COALESCE(lease_expires_at, 0),
			COALESCE(last_reconciled_at, 0)
		FROM channels
		WHERE id = ?`,
		channelID,
	)

	var channel ChannelRecord

	err := row.Scan(
		&channel.ID,
		&channel.Title,
		&channel.UploadsPlaylistID,
		&channel.WebSubSecret,
		&channel.LeaseStartedAt,
		&channel.LeaseExpiresAt,
		&channel.LastReconciledAt,
	)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}

		return nil, fmt.Errorf("get channel %s: %w", channelID, err)
	}

	return &channel, nil
}

func (d *Database) Channels(ctx context.Context) ([]ChannelRecord, error) {
	rows, err := d.db.QueryContext(
		ctx,
		`SELECT
			id,
			title,
			uploads_playlist_id,
			websub_secret,
			COALESCE(lease_started_at, 0),
			COALESCE(lease_expires_at, 0),
			COALESCE(last_reconciled_at, 0)
		FROM channels`,
	)

	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}

	defer rows.Close()

	channels := make([]ChannelRecord, 0)

	for rows.Next() {
		var channel ChannelRecord

		err = rows.Scan(
			&channel.ID,
			&channel.Title,
			&channel.UploadsPlaylistID,
			&channel.WebSubSecret,
			&channel.LeaseStartedAt,
			&channel.LeaseExpiresAt,
			&channel.LastReconciledAt,
		)

		if err != nil {
			return nil, fmt.Errorf("scan channel: %w", err)
		}

		channels = append(channels, channel)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("iterate channels: %w", err)
	}

	return channels, nil
}

func (d *Database) UpdateChannelMetadata(ctx context.Context, metadata ChannelMetadata) error {
	_, err := d.db.ExecContext(
		ctx,
		`UPDATE channels SET title = ?, uploads_playlist_id = ? WHERE id = ?`,
		metadata.Title,
		metadata.UploadsPlaylistID,
		metadata.ID,
	)

	if err != nil {
		return fmt.Errorf("update channel %s metadata: %w", metadata.ID, err)
	}

	return nil
}

func (d *Database) SetSubscriptionLease(ctx context.Context, channelID string, startedAt, expiresAt time.Time) error {
	_, err := d.db.ExecContext(
		ctx,
		`UPDATE channels SET lease_started_at = ?, lease_expires_at = ? WHERE id = ?`,
		startedAt.Unix(),
		expiresAt.Unix(),
		channelID,
	)

	if err != nil {
		return fmt.Errorf("update channel %s subscription lease: %w", channelID, err)
	}

	return nil
}

func (d *Database) ClearSubscriptionLease(ctx context.Context, channelID string) error {
	_, err := d.db.ExecContext(
		ctx,
		`UPDATE channels SET lease_started_at = NULL, lease_expires_at = NULL WHERE id = ?`,
		channelID,
	)

	if err != nil {
		return fmt.Errorf("clear channel %s subscription lease: %w", channelID, err)
	}

	return nil
}

func (d *Database) SetLastReconciled(ctx context.Context, channelID string, reconciledAt time.Time) error {
	_, err := d.db.ExecContext(
		ctx,
		`UPDATE channels SET last_reconciled_at = ? WHERE id = ?`,
		reconciledAt.Unix(),
		channelID,
	)

	if err != nil {
		return fmt.Errorf("update channel %s reconciliation time: %w", channelID, err)
	}

	return nil
}

func (d *Database) DeleteChannel(ctx context.Context, channelID string) error {
	transaction, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin channel deletion: %w", err)
	}

	defer transaction.Rollback()

	_, err = transaction.ExecContext(ctx, `DELETE FROM videos WHERE channel_id = ?`, channelID)
	if err != nil {
		return fmt.Errorf("delete channel %s videos: %w", channelID, err)
	}

	_, err = transaction.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, channelID)
	if err != nil {
		return fmt.Errorf("delete channel %s: %w", channelID, err)
	}

	err = transaction.Commit()
	if err != nil {
		return fmt.Errorf("commit channel %s deletion: %w", channelID, err)
	}

	return nil
}

func (d *Database) DeleteVideo(ctx context.Context, videoID string) error {
	_, err := d.db.ExecContext(ctx, `DELETE FROM videos WHERE id = ?`, videoID)
	if err != nil {
		return fmt.Errorf("delete video %s: %w", videoID, err)
	}

	return nil
}

func (d *Database) VideoExists(ctx context.Context, videoID string) (bool, error) {
	var exists bool

	err := d.db.QueryRowContext(
		ctx,
		`SELECT EXISTS(SELECT 1 FROM videos WHERE id = ?)`,
		videoID,
	).Scan(&exists)

	if err != nil {
		return false, fmt.Errorf("check video %s: %w", videoID, err)
	}

	return exists, nil
}

func (d *Database) UpsertVideo(ctx context.Context, video VideoRecord) error {
	kind := video.Kind
	if kind == "" {
		kind = VideoKindUnknown
	}

	_, err := d.db.ExecContext(
		ctx,
		`INSERT INTO videos (id, channel_id, title, published_at, updated_at, kind, member_only)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			channel_id = excluded.channel_id,
			title = excluded.title,
			published_at = excluded.published_at,
			updated_at = excluded.updated_at,
			member_only = excluded.member_only`,
		video.ID,
		video.ChannelID,
		video.Title,
		video.PublishedAt,
		video.UpdatedAt,
		kind,
		video.MemberOnly,
	)

	if err != nil {
		return fmt.Errorf("upsert video %s: %w", video.ID, err)
	}

	return nil
}

func (d *Database) PendingVideos(ctx context.Context, channelID string) ([]PendingVideo, error) {
	rows, err := d.db.QueryContext(
		ctx,
		`SELECT id, published_at
		FROM videos
		WHERE channel_id = ? AND kind = ?
		ORDER BY published_at ASC`,
		channelID,
		VideoKindUnknown,
	)

	if err != nil {
		return nil, fmt.Errorf("list pending videos for %s: %w", channelID, err)
	}

	defer rows.Close()

	videos := make([]PendingVideo, 0)

	for rows.Next() {
		var video PendingVideo

		err = rows.Scan(&video.ID, &video.PublishedAt)
		if err != nil {
			return nil, fmt.Errorf("scan pending video: %w", err)
		}

		videos = append(videos, video)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("iterate pending videos: %w", err)
	}

	return videos, nil
}

func (d *Database) PendingClassificationChannels(ctx context.Context) ([]string, error) {
	rows, err := d.db.QueryContext(
		ctx,
		`SELECT DISTINCT channel_id FROM videos WHERE kind = ?`,
		VideoKindUnknown,
	)

	if err != nil {
		return nil, fmt.Errorf("list pending classification channels: %w", err)
	}

	defer rows.Close()

	channelIDs := make([]string, 0)

	for rows.Next() {
		var channelID string

		err = rows.Scan(&channelID)
		if err != nil {
			return nil, fmt.Errorf("scan pending classification channel: %w", err)
		}

		channelIDs = append(channelIDs, channelID)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("iterate pending classification channels: %w", err)
	}

	return channelIDs, nil
}

func (d *Database) UpdateVideoKinds(ctx context.Context, updates []VideoKindUpdate) error {
	if len(updates) == 0 {
		return nil
	}

	transaction, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin video classification update: %w", err)
	}

	defer transaction.Rollback()

	statement, err := transaction.PrepareContext(ctx, `UPDATE videos SET kind = ? WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("prepare video classification update: %w", err)
	}

	defer statement.Close()

	for _, update := range updates {
		_, err = statement.ExecContext(ctx, update.Kind, update.VideoID)
		if err != nil {
			return fmt.Errorf("update video %s kind: %w", update.VideoID, err)
		}
	}

	err = transaction.Commit()
	if err != nil {
		return fmt.Errorf("commit video classification update: %w", err)
	}

	return nil
}

func (d *Database) ListVideos(ctx context.Context, channelIDs []string, includeShorts bool, limit int) ([]VideoRecord, error) {
	if len(channelIDs) == 0 {
		return []VideoRecord{}, nil
	}

	argumentCapacity := len(channelIDs) + 1
	queryCapacity := len(listVideosQueryPrefix) + len(listVideosQuerySuffix) + len(channelIDs)*2

	if !includeShorts {
		argumentCapacity++
		queryCapacity += len(listVideosKindFilter)
	}

	arguments := make([]any, 0, argumentCapacity)

	var query strings.Builder

	query.Grow(queryCapacity)

	query.WriteString(listVideosQueryPrefix)

	for index, channelID := range channelIDs {
		if index > 0 {
			query.WriteByte(',')
		}

		query.WriteByte('?')

		arguments = append(arguments, channelID)
	}

	query.WriteByte(')')

	if !includeShorts {
		query.WriteString(listVideosKindFilter)

		arguments = append(arguments, VideoKindVideo)
	}

	query.WriteString(listVideosQuerySuffix)

	arguments = append(arguments, limit)

	rows, err := d.db.QueryContext(ctx, query.String(), arguments...)
	if err != nil {
		return nil, fmt.Errorf("list videos: %w", err)
	}

	defer rows.Close()

	videos := make([]VideoRecord, 0, limit)

	for rows.Next() {
		var video VideoRecord

		err = rows.Scan(
			&video.ID,
			&video.ChannelID,
			&video.ChannelTitle,
			&video.Title,
			&video.PublishedAt,
			&video.UpdatedAt,
			&video.Kind,
			&video.MemberOnly,
		)

		if err != nil {
			return nil, fmt.Errorf("scan video: %w", err)
		}

		videos = append(videos, video)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("iterate videos: %w", err)
	}

	return videos, nil
}

func OpenDatabase() (*Database, error) {
	err := os.MkdirAll(filepath.Dir(DatabasePath), 0o755)
	if err != nil {
		return nil, err
	}

	databaseDSN := fmt.Sprintf("%s?_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL&_synchronous=NORMAL", DatabasePath)

	database, err := sql.Open("sqlite3", databaseDSN)
	if err != nil {
		return nil, err
	}

	err = database.Ping()
	if err != nil {
		database.Close()

		return nil, err
	}

	err = ApplySchema(database)
	if err != nil {
		database.Close()

		return nil, err
	}

	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)

	return &Database{database}, nil
}

func ApplySchema(database *sql.DB) error {
	schema, err := schgo.NewSchema(database)
	if err != nil {
		return fmt.Errorf("create schema: %w", err)
	}

	// channels
	table := schema.Table("channels")

	table.Primary("id", "TEXT")

	table.Column("title", "TEXT").NotNull().Default("")
	table.Column("uploads_playlist_id", "TEXT").NotNull().Default("")
	table.Column("websub_secret", "TEXT").NotNull().Default("")
	table.Column("lease_started_at", "INTEGER").Null()
	table.Column("lease_expires_at", "INTEGER").Null()
	table.Column("last_reconciled_at", "INTEGER").Null()

	table.Index("idx_channels_lease_expires_at", "lease_expires_at")

	// videos
	table = schema.Table("videos")

	table.Primary("id", "TEXT")

	table.Column("channel_id", "TEXT").NotNull()
	table.Column("title", "TEXT").NotNull().Default("")
	table.Column("published_at", "INTEGER").NotNull()
	table.Column("updated_at", "INTEGER").NotNull()
	table.Column("kind", "TEXT").NotNull().Default(string(VideoKindUnknown))
	table.Column("member_only", "INTEGER").NotNull().Default("0")

	table.Index("idx_videos_published_at", "published_at")
	table.Index("idx_videos_channel_published_at", "channel_id", "published_at")
	table.Index("idx_videos_kind", "kind")

	err = schema.Apply()
	if err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}

	return nil
}

func randomSecret() (string, error) {
	buffer := make([]byte, 32)

	_, err := rand.Read(buffer)
	if err != nil {
		return "", fmt.Errorf("generate websub secret: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(buffer), nil
}
