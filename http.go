package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

const (
	defaultFeedLimit       = 50
	maximumFeedLimit       = 200
	maximumWebhookBodySize = 1 << 20
)

type HTTPHandler struct {
	service *Service
}

type FeedResponse struct {
	Videos []FeedVideo `json:"videos"`
}

type FeedVideo struct {
	ID          string    `json:"id"`
	ChannelID   string    `json:"channelId"`
	Channel     string    `json:"channel"`
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	Thumbnail   string    `json:"thumbnail"`
	PublishedAt time.Time `json:"publishedAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Kind        VideoKind `json:"kind"`
	Groups      []string  `json:"groups"`
}

func NewRouter(service *Service) http.Handler {
	handler := &HTTPHandler{
		service: service,
	}

	router := chi.NewRouter()

	router.Use(middleware.Recoverer)
	router.Use(log.Middleware())

	router.Get("/healthz", handler.Health)

	router.Get("/api/videos", handler.Videos)
	router.Get("/api/groups/{group}/videos", handler.GroupVideos)

	router.Get("/websub/{channelID}/{token}", handler.VerifySubscription)
	router.Post("/websub/{channelID}/{token}", handler.Notification)

	return router
}

func (h *HTTPHandler) Health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)

	w.Write([]byte("meow"))
}

func (h *HTTPHandler) Videos(w http.ResponseWriter, r *http.Request) {
	limit, err := feedLimit(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	videos, err := db.ListVideos(
		r.Context(),
		cfg.ChannelIDs(),
		cfg.YouTube.IncludeShorts,
		limit,
	)

	if err != nil {
		log.Errorf("list feed videos: %v\n", err)

		http.Error(w, "failed to load videos", http.StatusInternalServerError)

		return
	}

	h.writeFeed(w, videos)
}

func (h *HTTPHandler) GroupVideos(w http.ResponseWriter, r *http.Request) {
	group := cfg.Group(chi.URLParam(r, "group"))
	if group == nil {
		http.NotFound(w, r)

		return
	}

	limit, err := feedLimit(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	videos, err := db.ListVideos(
		r.Context(),
		group.Channels,
		cfg.YouTube.IncludeShorts,
		limit,
	)

	if err != nil {
		log.Errorf("list group %s videos: %v\n", group.Slug, err)

		http.Error(w, "failed to load videos", http.StatusInternalServerError)

		return
	}

	h.writeFeed(w, videos)
}

func (h *HTTPHandler) writeFeed(w http.ResponseWriter, videos []VideoRecord) {
	feedVideos := make([]FeedVideo, 0, len(videos))

	for _, video := range videos {
		feedVideos = append(feedVideos, FeedVideo{
			ID:          video.ID,
			ChannelID:   video.ChannelID,
			Channel:     video.ChannelTitle,
			Title:       video.Title,
			URL:         youtubeWebBaseURL + "/watch?v=" + video.ID,
			Thumbnail:   "https://i.ytimg.com/vi/" + video.ID + "/hqdefault.jpg",
			PublishedAt: time.Unix(video.PublishedAt, 0).UTC(),
			UpdatedAt:   time.Unix(video.UpdatedAt, 0).UTC(),
			Kind:        video.Kind,
			Groups:      cfg.GroupNames(video.ChannelID),
		})
	}

	writeJSON(w, http.StatusOK, FeedResponse{Videos: feedVideos})
}

func (h *HTTPHandler) VerifySubscription(w http.ResponseWriter, r *http.Request) {
	channelID := chi.URLParam(r, "channelID")
	token := chi.URLParam(r, "token")

	channel, err := db.Channel(r.Context(), channelID)
	if err != nil {
		log.Warnf("load websub channel %s: %v\n", channelID, err)

		http.Error(w, "subscription unavailable", http.StatusServiceUnavailable)

		return
	}

	if channel == nil || !verifyCallbackToken(channel.WebSubSecret, token) {
		http.NotFound(w, r)

		return
	}

	mode := r.URL.Query().Get("hub.mode")
	topic := r.URL.Query().Get("hub.topic")
	challenge := r.URL.Query().Get("hub.challenge")

	if topic != h.service.websub.TopicURL(channelID) {
		http.Error(w, "invalid verification request", http.StatusBadRequest)

		return
	}

	if mode == "denied" {
		log.Warnf("subscription denied for %s: %s\n", channelID, r.URL.Query().Get("hub.reason"))

		w.WriteHeader(http.StatusNoContent)

		return
	}

	if !validWebSubChallenge(challenge) {
		http.Error(w, "invalid verification challenge", http.StatusBadRequest)

		return
	}

	switch mode {
	case "subscribe":
		if !cfg.HasChannel(channelID) {
			http.Error(w, "channel is no longer configured", http.StatusNotFound)

			return
		}

		lease, err := parseLeaseSeconds(r.URL.Query().Get("hub.lease_seconds"))
		if err != nil {
			http.Error(w, "invalid lease", http.StatusBadRequest)

			return
		}

		now := time.Now()

		err = db.SetSubscriptionLease(r.Context(), channelID, now, now.Add(lease))
		if err != nil {
			log.Warnf("store subscription lease for %s: %v\n", channelID, err)

			http.Error(w, "subscription unavailable", http.StatusServiceUnavailable)

			return
		}

		log.Printf("subscription active for %s until %s\n", channelID, now.Add(lease).Format(time.RFC3339))
	case "unsubscribe":
		if cfg.HasChannel(channelID) {
			err = db.ClearSubscriptionLease(r.Context(), channelID)
		} else {
			err = db.DeleteChannel(r.Context(), channelID)
		}

		if err != nil {
			log.Warnf("complete unsubscribe for %s: %v\n", channelID, err)

			http.Error(w, "subscription unavailable", http.StatusServiceUnavailable)

			return
		}

		log.Printf("subscription removed for %s\n", channelID)
	default:
		http.Error(w, "invalid verification mode", http.StatusBadRequest)

		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	_, _ = w.Write([]byte(challenge))
}

func (h *HTTPHandler) Notification(w http.ResponseWriter, r *http.Request) {
	channelID := chi.URLParam(r, "channelID")
	token := chi.URLParam(r, "token")

	channel, err := db.Channel(r.Context(), channelID)
	if err != nil {
		log.Warnf("load notification channel %s: %v\n", channelID, err)
		w.WriteHeader(http.StatusAccepted)

		return
	}

	if channel == nil || !verifyCallbackToken(channel.WebSubSecret, token) {
		w.WriteHeader(http.StatusAccepted)

		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maximumWebhookBodySize+1))
	if err != nil {
		log.Warnf("read notification for %s: %v\n", channelID, err)

		w.WriteHeader(http.StatusAccepted)

		return
	}

	if len(body) > maximumWebhookBodySize {
		log.Warnf("notification for %s exceeded maximum body size\n", channelID)

		w.WriteHeader(http.StatusAccepted)

		return
	}

	if !verifyWebSubSignature(channel.WebSubSecret, r.Header.Get("X-Hub-Signature"), body) {
		log.Warnf("ignored notification with invalid signature for %s\n", channelID)

		w.WriteHeader(http.StatusAccepted)

		return
	}

	if !cfg.HasChannel(channelID) {
		w.WriteHeader(http.StatusAccepted)

		return
	}

	err = h.service.HandleNotification(r.Context(), channelID, body)
	if err != nil {
		log.Warnf("process notification for %s: %v\n", channelID, err)
	}

	w.WriteHeader(http.StatusAccepted)
}

func validWebSubChallenge(challenge string) bool {
	if challenge == "" || len(challenge) > 1024 {
		return false
	}

	for index := range len(challenge) {
		char := challenge[index]

		if char == '+' || char == '=' || char == '_' {
			continue
		}

		if char >= '-' && char <= '9' {
			continue
		}

		if char >= 'A' && char <= 'Z' {
			continue
		}

		if char >= 'a' && char <= 'z' {
			continue
		}

		return false
	}

	return true
}

func feedLimit(r *http.Request) (int, error) {
	value := r.URL.Query().Get("limit")
	if value == "" {
		return defaultFeedLimit, nil
	}

	limit, err := strconv.Atoi(value)
	if err != nil || limit <= 0 {
		return 0, fmt.Errorf("limit must be a positive integer")
	}

	if limit > maximumFeedLimit {
		limit = maximumFeedLimit
	}

	return limit, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	encoder := json.NewEncoder(w)

	encoder.SetEscapeHTML(false)

	_ = encoder.Encode(value)
}

func ShutdownServer(server *http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return server.Shutdown(ctx)
}
