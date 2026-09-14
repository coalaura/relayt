package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	youtubeAPIBaseURL  = "https://www.googleapis.com/youtube/v3"
	youtubeWebBaseURL  = "https://www.youtube.com"
	youtubeAPIAttempts = 4
)

type YouTubeClient struct {
	apiKey     string
	apiBaseURL string
	apiClient  *http.Client
}

type youtubeChannelListResponse struct {
	Items []youtubeChannel `json:"items"`
}

type youtubeChannel struct {
	ID             string                       `json:"id"`
	Snippet        youtubeChannelSnippet        `json:"snippet"`
	ContentDetails youtubeChannelContentDetails `json:"contentDetails"`
}

type youtubeChannelSnippet struct {
	Title string `json:"title"`
}

type youtubeChannelContentDetails struct {
	RelatedPlaylists youtubeRelatedPlaylists `json:"relatedPlaylists"`
}

type youtubeRelatedPlaylists struct {
	Uploads string `json:"uploads"`
}

type youtubePlaylistItemsResponse struct {
	NextPageToken string                `json:"nextPageToken"`
	Items         []youtubePlaylistItem `json:"items"`
}

type youtubePlaylistItem struct {
	Snippet        youtubePlaylistItemSnippet        `json:"snippet"`
	ContentDetails youtubePlaylistItemContentDetails `json:"contentDetails"`
}

type youtubePlaylistItemSnippet struct {
	Title       string `json:"title"`
	PublishedAt string `json:"publishedAt"`
}

type youtubePlaylistItemContentDetails struct {
	VideoID          string `json:"videoId"`
	VideoPublishedAt string `json:"videoPublishedAt"`
}

type youtubeErrorResponse struct {
	Error youtubeError `json:"error"`
}

type youtubeError struct {
	Errors []youtubeErrorDetail `json:"errors"`
}

type youtubeErrorDetail struct {
	Reason string `json:"reason"`
}

var errYouTubePlaylistNotFound = errors.New("youtube playlist not found")

func NewYouTubeClient(apiKey string) *YouTubeClient {
	return &YouTubeClient{
		apiKey:     apiKey,
		apiBaseURL: youtubeAPIBaseURL,
		apiClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

func (c *YouTubeClient) Channels(ctx context.Context, channelIDs []string) ([]ChannelMetadata, error) {
	if len(channelIDs) == 0 {
		return []ChannelMetadata{}, nil
	}

	query := url.Values{}

	query.Set("part", "snippet,contentDetails")
	query.Set("id", strings.Join(channelIDs, ","))
	query.Set("fields", "items(id,snippet/title,contentDetails/relatedPlaylists/uploads)")

	var response youtubeChannelListResponse

	err := c.getJSON(ctx, "/channels", query, &response)
	if err != nil {
		return nil, err
	}

	channels := make([]ChannelMetadata, 0, len(response.Items))

	for _, channel := range response.Items {
		channels = append(channels, ChannelMetadata{
			ID:                channel.ID,
			Title:             channel.Snippet.Title,
			UploadsPlaylistID: channel.ContentDetails.RelatedPlaylists.Uploads,
		})
	}

	return channels, nil
}

func (c *YouTubeClient) PlaylistItems(ctx context.Context, playlistID, pageToken string) (*PlaylistPage, error) {
	query := url.Values{}

	query.Set("part", "snippet,contentDetails")
	query.Set("playlistId", playlistID)
	query.Set("maxResults", "50")
	query.Set("fields", "nextPageToken,items(snippet/title,snippet/publishedAt,contentDetails/videoId,contentDetails/videoPublishedAt)")

	if pageToken != "" {
		query.Set("pageToken", pageToken)
	}

	var response youtubePlaylistItemsResponse

	err := c.getJSON(ctx, "/playlistItems", query, &response)
	if err != nil {
		return nil, err
	}

	page := &PlaylistPage{
		Items:         make([]PlaylistItem, 0, len(response.Items)),
		NextPageToken: response.NextPageToken,
	}

	for _, item := range response.Items {
		addedAt, err := time.Parse(time.RFC3339, item.Snippet.PublishedAt)
		if err != nil {
			return nil, fmt.Errorf("parse playlist item %s time: %w", item.ContentDetails.VideoID, err)
		}

		publishedAt := item.ContentDetails.VideoPublishedAt
		if publishedAt == "" {
			publishedAt = item.Snippet.PublishedAt
		}

		published, err := time.Parse(time.RFC3339, publishedAt)
		if err != nil {
			return nil, fmt.Errorf("parse video %s publication time: %w", item.ContentDetails.VideoID, err)
		}

		page.Items = append(page.Items, PlaylistItem{
			VideoID:     item.ContentDetails.VideoID,
			Title:       item.Snippet.Title,
			AddedAt:     addedAt,
			PublishedAt: published,
		})
	}

	return page, nil
}

func (c *YouTubeClient) ShortsSince(ctx context.Context, channelID string, since time.Time) (map[string]struct{}, error) {
	playlistID, err := shortsPlaylistID(channelID)
	if err != nil {
		return nil, err
	}

	shorts := make(map[string]struct{})
	pageToken := ""

	for {
		query := url.Values{}

		query.Set("part", "contentDetails")
		query.Set("playlistId", playlistID)
		query.Set("maxResults", "50")
		query.Set("fields", "nextPageToken,items(contentDetails/videoId,contentDetails/videoPublishedAt)")

		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}

		var response youtubePlaylistItemsResponse

		err = c.getJSON(ctx, "/playlistItems", query, &response)
		if err != nil {
			if errors.Is(err, errYouTubePlaylistNotFound) {
				return shorts, nil
			}

			return nil, fmt.Errorf("load shorts playlist: %w", err)
		}

		oldestPublishedAt := time.Time{}

		for _, item := range response.Items {
			if item.ContentDetails.VideoID == "" {
				continue
			}

			publishedAt, err := time.Parse(time.RFC3339, item.ContentDetails.VideoPublishedAt)
			if err != nil {
				return nil, fmt.Errorf("parse short %s publication time: %w", item.ContentDetails.VideoID, err)
			}

			shorts[item.ContentDetails.VideoID] = struct{}{}

			if oldestPublishedAt.IsZero() || publishedAt.Before(oldestPublishedAt) {
				oldestPublishedAt = publishedAt
			}
		}

		if response.NextPageToken == "" {
			break
		}

		if !since.IsZero() && !oldestPublishedAt.IsZero() && !oldestPublishedAt.After(since) {
			break
		}

		pageToken = response.NextPageToken
	}

	return shorts, nil
}

func (c *YouTubeClient) getJSON(ctx context.Context, path string, query url.Values, target any) error {
	query.Set("key", c.apiKey)

	requestURL := c.apiBaseURL + path + "?" + query.Encode()

	var lastErr error

	for attempt := range youtubeAPIAttempts {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
		if err != nil {
			return fmt.Errorf("create youtube api request: %w", err)
		}

		request.Header.Set("Accept", "application/json")

		response, err := c.apiClient.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			lastErr = fmt.Errorf("youtube api request: %w", err)

			if attempt == youtubeAPIAttempts-1 {
				return lastErr
			}

			if !waitForYouTubeRetry(ctx, attempt, "") {
				return ctx.Err()
			}

			continue
		}

		if response.StatusCode >= 200 && response.StatusCode < 300 {
			decoder := json.NewDecoder(response.Body)

			err = decoder.Decode(target)

			response.Body.Close()

			if err != nil {
				return fmt.Errorf("decode youtube api response: %w", err)
			}

			return nil
		}

		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))

		response.Body.Close()

		lastErr = fmt.Errorf("youtube api returned %s: %s", response.Status, strings.TrimSpace(string(body)))

		if response.StatusCode == http.StatusNotFound && isYouTubePlaylistNotFound(body) {
			return fmt.Errorf("%w: %v", errYouTubePlaylistNotFound, lastErr)
		}

		if !isRetryableYouTubeStatus(response.StatusCode) || attempt == youtubeAPIAttempts-1 {
			return lastErr
		}

		if !waitForYouTubeRetry(ctx, attempt, response.Header.Get("Retry-After")) {
			return ctx.Err()
		}
	}

	return lastErr
}

func shortsPlaylistID(channelID string) (string, error) {
	if len(channelID) <= 2 || !strings.HasPrefix(channelID, "UC") {
		return "", fmt.Errorf("invalid youtube channel id %q", channelID)
	}

	return "UUSH" + channelID[2:], nil
}

func isRetryableYouTubeStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func isYouTubePlaylistNotFound(body []byte) bool {
	var response youtubeErrorResponse

	err := json.Unmarshal(body, &response)
	if err != nil {
		return false
	}

	for _, detail := range response.Error.Errors {
		if detail.Reason == "playlistNotFound" {
			return true
		}
	}

	return false
}

func waitForYouTubeRetry(ctx context.Context, attempt int, retryAfter string) bool {
	delay := time.Duration(1<<attempt) * time.Second

	if retryAfter != "" {
		seconds, err := strconv.Atoi(retryAfter)
		if err == nil && seconds >= 0 {
			delay = time.Duration(seconds) * time.Second
		}
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
