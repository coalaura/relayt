package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	reconciliationInterval      = time.Hour
	subscriptionCheckInterval   = time.Hour
	subscriptionRenewalFraction = 0.75
	classificationBatchDelay    = 2 * time.Second
	classificationQueueSize     = 256
	maxYouTubeBatchSize         = 50
)

type playlistTimestamp int

const (
	playlistVideoPublishedAt playlistTimestamp = iota
	playlistItemAddedAt
)

type Service struct {
	youtube *YouTubeClient
	websub  *WebSubClient

	classificationQueue chan string
	waitGroup           sync.WaitGroup
}

type youtubeNotificationFeed struct {
	Entries        []youtubeNotificationEntry `xml:"entry"`
	DeletedEntries []youtubeDeletedEntry      `xml:"deleted-entry"`
}

type youtubeNotificationEntry struct {
	VideoID   string `xml:"videoId"`
	ChannelID string `xml:"channelId"`
	Title     string `xml:"title"`
	Published string `xml:"published"`
	Updated   string `xml:"updated"`
}

type youtubeDeletedEntry struct {
	Ref string           `xml:"ref,attr"`
	By  youtubeDeletedBy `xml:"by"`
}

type youtubeDeletedBy struct {
	URI string `xml:"uri"`
}

func NewService() *Service {
	return &Service{
		youtube: NewYouTubeClient(cfg.YouTube.APIKey),
		websub:  NewWebSubClient(cfg.Server.PublicURL),

		classificationQueue: make(chan string, classificationQueueSize),
	}
}

func (s *Service) Start(ctx context.Context) {
	s.waitGroup.Add(1)

	go s.classificationLoop(ctx)

	s.ensureConfiguredChannels(ctx)
	s.refreshChannelMetadata(ctx)
	s.reconcileAll(ctx)

	log.Println("initial reconciliation finished")

	s.waitGroup.Add(2)

	go s.subscriptionLoop(ctx)
	go s.reconciliationLoop(ctx)
}

func (s *Service) Wait() {
	s.waitGroup.Wait()
}

func (s *Service) ensureConfiguredChannels(ctx context.Context) {
	channelIDs := cfg.ChannelIDs()

	for _, channelID := range channelIDs {
		err := db.EnsureChannel(ctx, channelID)
		if err != nil {
			log.Errorf("ensure channel %s: %v\n", channelID, err)
		}
	}
}

func (s *Service) cleanupRemovedChannels(ctx context.Context) {
	channels, err := db.Channels(ctx)
	if err != nil {
		log.Errorf("load stored channels: %v\n", err)

		return
	}

	now := time.Now()

	for _, channel := range channels {
		if cfg.HasChannel(channel.ID) {
			continue
		}

		if channel.LeaseExpiresAt == 0 || channel.LeaseExpiresAt <= now.Unix() {
			err = db.DeleteChannel(ctx, channel.ID)
			if err != nil {
				log.Errorf("delete removed channel %s: %v\n", channel.ID, err)
			}

			continue
		}

		err = s.websub.Unsubscribe(ctx, channel.ID, channel.WebSubSecret)
		if err != nil {
			log.Warnf("unsubscribe removed channel %s: %v\n", channel.ID, err)

			continue
		}

		log.Printf("requested unsubscribe for removed channel %s\n", channel.ID)
	}
}

func (s *Service) ensureSubscriptions(ctx context.Context) {
	if !cfg.YouTube.Subscribe {
		return
	}

	channelIDs := cfg.ChannelIDs()

	now := time.Now()

	for _, channelID := range channelIDs {
		channel, err := db.Channel(ctx, channelID)
		if err != nil {
			log.Warnf("load channel %s subscription state: %v\n", channelID, err)

			continue
		}

		if channel == nil {
			continue
		}

		if !subscriptionNeedsRenewal(channel, now) {
			continue
		}

		err = s.websub.Subscribe(ctx, channel.ID, channel.WebSubSecret)
		if err != nil {
			log.Warnf("subscribe channel %s: %v\n", channel.ID, err)

			continue
		}

		log.Printf("requested subscription for %s\n", channel.ID)
	}
}

func (s *Service) refreshChannelMetadata(ctx context.Context) {
	channelIDs := cfg.ChannelIDs()

	for start := 0; start < len(channelIDs); start += maxYouTubeBatchSize {
		end := min(start+maxYouTubeBatchSize, len(channelIDs))
		batch := channelIDs[start:end]

		metadata, err := s.youtube.Channels(ctx, batch)
		if err != nil {
			log.Warnf("refresh channel metadata: %v\n", err)

			continue
		}

		returned := make(map[string]struct{}, len(metadata))

		for _, channel := range metadata {
			returned[channel.ID] = struct{}{}

			err = db.UpdateChannelMetadata(ctx, channel)
			if err != nil {
				log.Warnf("store channel %s metadata: %v\n", channel.ID, err)
			}
		}

		for _, channelID := range batch {
			if _, exists := returned[channelID]; !exists {
				log.Warnf("youtube did not return configured channel %s\n", channelID)
			}
		}
	}
}

func (s *Service) reconcileAll(ctx context.Context) {
	channelIDs := cfg.ChannelIDs()

	for _, channelID := range channelIDs {
		if ctx.Err() != nil {
			return
		}

		err := s.reconcileChannel(ctx, channelID)
		if err != nil {
			log.Warnf("reconcile channel %s: %v\n", channelID, err)
		}
	}
}

func (s *Service) reconcileChannel(ctx context.Context, channelID string) error {
	channel, err := db.Channel(ctx, channelID)
	if err != nil {
		return err
	}

	if channel == nil {
		return fmt.Errorf("channel is not stored")
	}

	if channel.UploadsPlaylistID == "" {
		return fmt.Errorf("uploads playlist is unknown")
	}

	newVideos, err := s.reconcilePlaylist(ctx, channelID, channel.UploadsPlaylistID, channel.LastReconciledAt, VideoKindUnknown, playlistVideoPublishedAt)
	if err != nil {
		return err
	}

	if cfg.YouTube.IncludeMemberVideos {
		playlistID, playlistErr := membersPlaylistID(channelID)
		if playlistErr != nil {
			return playlistErr
		}

		memberVideos, reconcileErr := s.reconcilePlaylist(ctx, channelID, playlistID, channel.LastReconciledAt, VideoKindVideo, playlistItemAddedAt)
		if reconcileErr == nil {
			newVideos += memberVideos
		} else if !errors.Is(reconcileErr, errYouTubePlaylistNotFound) {
			log.Warnf("reconcile member videos for %s: %v\n", channelID, reconcileErr)
		}
	}

	err = db.SetLastReconciled(ctx, channelID, time.Now())
	if err != nil {
		return err
	}

	if newVideos > 0 {
		log.Printf("reconciled %s: %d new videos\n", channelID, newVideos)
	}

	err = s.classifyPendingChannel(ctx, channelID)
	if err != nil {
		log.Warnf("classify pending videos for %s: %v\n", channelID, err)
	}

	return nil
}

func (s *Service) reconcilePlaylist(ctx context.Context, channelID string, playlistID string, lastReconciledAt int64, kind VideoKind, timestamp playlistTimestamp) (int, error) {
	initialSync := lastReconciledAt == 0
	pageToken := ""
	newVideos := 0

	for {
		page, err := s.youtube.PlaylistItems(ctx, playlistID, pageToken)
		if err != nil {
			return 0, err
		}

		foundReconciliationBoundary := false

		for _, item := range page.Items {
			if item.VideoID == "" {
				continue
			}

			existed, err := db.VideoExists(ctx, item.VideoID)
			if err != nil {
				return 0, err
			}

			video := playlistVideoRecord(channelID, item, kind, timestamp)

			err = db.UpsertVideo(ctx, video)
			if err != nil {
				return 0, err
			}

			if !initialSync && item.AddedAt.Unix() <= lastReconciledAt {
				foundReconciliationBoundary = true
			}

			if !existed {
				newVideos++
			}
		}

		if initialSync || foundReconciliationBoundary || page.NextPageToken == "" {
			break
		}

		pageToken = page.NextPageToken
	}

	return newVideos, nil
}

func playlistVideoRecord(channelID string, item PlaylistItem, kind VideoKind, timestamp playlistTimestamp) VideoRecord {
	publishedAt := item.PublishedAt

	if timestamp == playlistItemAddedAt {
		publishedAt = item.AddedAt
	}

	return VideoRecord{
		ID:          item.VideoID,
		ChannelID:   channelID,
		Title:       item.Title,
		PublishedAt: publishedAt.Unix(),
		UpdatedAt:   publishedAt.Unix(),
		Kind:        kind,
	}
}

func (s *Service) queuePendingChannels(ctx context.Context) {
	channelIDs, err := db.PendingClassificationChannels(ctx)
	if err != nil {
		log.Warnf("load pending classification channels: %v\n", err)

		return
	}

	for _, channelID := range channelIDs {
		if !cfg.HasChannel(channelID) {
			continue
		}

		s.queueClassification(ctx, channelID)
	}
}

func (s *Service) queueClassification(ctx context.Context, channelID string) {
	select {
	case s.classificationQueue <- channelID:
	case <-ctx.Done():
	}
}

func (s *Service) classificationLoop(ctx context.Context) {
	defer s.waitGroup.Done()

	for {
		var channelID string

		select {
		case <-ctx.Done():
			return
		case channelID = <-s.classificationQueue:
		}

		channelIDs := map[string]struct{}{
			channelID: {},
		}

		timer := time.NewTimer(classificationBatchDelay)

	collect:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()

				return
			case channelID = <-s.classificationQueue:
				channelIDs[channelID] = struct{}{}
			case <-timer.C:
				break collect
			}
		}

		batch := make([]string, 0, len(channelIDs))

		for channelID = range channelIDs {
			batch = append(batch, channelID)
		}

		sort.Strings(batch)

		for _, channelID = range batch {
			err := s.classifyPendingChannel(ctx, channelID)
			if err != nil {
				log.Warnf("classify pending videos for %s: %v\n", channelID, err)
			}
		}
	}
}

func (s *Service) classifyPendingChannel(ctx context.Context, channelID string) error {
	pending, err := db.PendingVideos(ctx, channelID)
	if err != nil {
		return err
	}

	if len(pending) == 0 {
		return nil
	}

	oldestPublishedAt := time.Unix(pending[0].PublishedAt, 0)

	shorts, err := s.youtube.ShortsSince(ctx, channelID, oldestPublishedAt)
	if err != nil {
		return err
	}

	updates := make([]VideoKindUpdate, 0, len(pending))
	shortCount := 0

	for _, video := range pending {
		kind := VideoKindVideo

		if _, exists := shorts[video.ID]; exists {
			kind = VideoKindShort
			shortCount++
		}

		updates = append(updates, VideoKindUpdate{
			VideoID: video.ID,
			Kind:    kind,
		})
	}

	err = db.UpdateVideoKinds(ctx, updates)
	if err != nil {
		return err
	}

	log.Printf("classified %s: %d videos, %d shorts\n", channelID, len(pending)-shortCount, shortCount)

	return nil
}

func (s *Service) subscriptionLoop(ctx context.Context) {
	defer s.waitGroup.Done()

	s.cleanupRemovedChannels(ctx)
	s.ensureSubscriptions(ctx)
	s.queuePendingChannels(ctx)

	ticker := time.NewTicker(subscriptionCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.cleanupRemovedChannels(ctx)
			s.ensureSubscriptions(ctx)
			s.queuePendingChannels(ctx)
		}
	}
}

func (s *Service) reconciliationLoop(ctx context.Context) {
	defer s.waitGroup.Done()

	ticker := time.NewTicker(reconciliationInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refreshChannelMetadata(ctx)
			s.reconcileAll(ctx)
		}
	}
}

func (s *Service) HandleNotification(ctx context.Context, channelID string, body []byte) error {
	var feed youtubeNotificationFeed

	err := xml.Unmarshal(body, &feed)
	if err != nil {
		return fmt.Errorf("parse youtube notification: %w", err)
	}

	for _, deleted := range feed.DeletedEntries {
		deletedChannelID := strings.TrimPrefix(deleted.By.URI, "yt:channel:")
		if deletedChannelID != "" && deletedChannelID != channelID {
			continue
		}

		videoID := strings.TrimPrefix(deleted.Ref, "yt:video:")
		if videoID == "" || videoID == deleted.Ref {
			continue
		}

		err = db.DeleteVideo(ctx, videoID)
		if err != nil {
			return err
		}

		log.Printf("removed deleted video %s from %s\n", videoID, channelID)
	}

	receivedVideo := false

	for _, entry := range feed.Entries {
		if entry.ChannelID != channelID {
			continue
		}

		if entry.VideoID == "" {
			continue
		}

		published, err := time.Parse(time.RFC3339, entry.Published)
		if err != nil {
			return fmt.Errorf("parse notification publication time: %w", err)
		}

		updated := published

		if entry.Updated != "" {
			parsedUpdated, parseErr := time.Parse(time.RFC3339, entry.Updated)
			if parseErr == nil {
				updated = parsedUpdated
			}
		}

		existed, err := db.VideoExists(ctx, entry.VideoID)
		if err != nil {
			return err
		}

		video := VideoRecord{
			ID:          entry.VideoID,
			ChannelID:   channelID,
			Title:       entry.Title,
			PublishedAt: published.Unix(),
			UpdatedAt:   updated.Unix(),
			Kind:        VideoKindUnknown,
		}

		err = db.UpsertVideo(ctx, video)
		if err != nil {
			return err
		}

		receivedVideo = true

		if !existed {
			log.Printf("received new video %s from %s\n", entry.VideoID, channelID)
		}
	}

	if receivedVideo {
		s.queueClassification(ctx, channelID)
	}

	return nil
}

func subscriptionNeedsRenewal(channel *ChannelRecord, now time.Time) bool {
	if channel.LeaseStartedAt == 0 || channel.LeaseExpiresAt == 0 {
		return true
	}

	if channel.LeaseExpiresAt <= now.Unix() {
		return true
	}

	leaseDuration := channel.LeaseExpiresAt - channel.LeaseStartedAt
	if leaseDuration <= 0 {
		return true
	}

	renewAt := float64(channel.LeaseStartedAt) + float64(leaseDuration)*subscriptionRenewalFraction

	return float64(now.Unix()) >= math.Floor(renewAt)
}

func parseLeaseSeconds(value string) (time.Duration, error) {
	seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse lease seconds: %w", err)
	}

	if seconds <= 0 {
		return 0, fmt.Errorf("lease seconds must be positive")
	}

	return time.Duration(seconds) * time.Second, nil
}
