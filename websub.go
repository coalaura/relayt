package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	webSubHubURL   = "https://pubsubhubbub.appspot.com/"
	webSubAttempts = 3
)

type WebSubClient struct {
	publicURL string
	client    *http.Client
}

func NewWebSubClient(publicURL string) *WebSubClient {
	return &WebSubClient{
		publicURL: strings.TrimRight(publicURL, "/"),
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

func (c *WebSubClient) Subscribe(ctx context.Context, channelID, secret string) error {
	return c.request(ctx, "subscribe", channelID, secret)
}

func (c *WebSubClient) Unsubscribe(ctx context.Context, channelID, secret string) error {
	return c.request(ctx, "unsubscribe", channelID, secret)
}

func (c *WebSubClient) CallbackURL(channelID, secret string) string {
	return c.publicURL + "/websub/" + url.PathEscape(channelID) + "/" + callbackToken(secret)
}

func (c *WebSubClient) TopicURL(channelID string) string {
	return youtubeWebBaseURL + "/feeds/videos.xml?channel_id=" + url.QueryEscape(channelID)
}

func (c *WebSubClient) request(ctx context.Context, mode, channelID, secret string) error {
	form := url.Values{}

	form.Set("hub.callback", c.CallbackURL(channelID, secret))
	form.Set("hub.mode", mode)
	form.Set("hub.topic", c.TopicURL(channelID))
	form.Set("hub.verify", "async")
	form.Set("hub.secret", secret)

	encodedForm := form.Encode()

	var lastErr error

	for attempt := range webSubAttempts {
		request, err := http.NewRequestWithContext(
			ctx,
			http.MethodPost,
			webSubHubURL,
			strings.NewReader(encodedForm),
		)

		if err != nil {
			return fmt.Errorf("create websub %s request: %w", mode, err)
		}

		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		response, err := c.client.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			lastErr = fmt.Errorf("send websub %s request: %w", mode, err)

			if attempt == webSubAttempts-1 {
				return lastErr
			}

			if !waitForWebSubRetry(ctx, attempt) {
				return ctx.Err()
			}

			continue
		}

		if response.StatusCode >= 200 && response.StatusCode < 300 {
			response.Body.Close()

			return nil
		}

		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))

		response.Body.Close()

		lastErr = fmt.Errorf("websub hub returned %s: %s", response.Status, strings.TrimSpace(string(body)))

		if (response.StatusCode != http.StatusTooManyRequests && response.StatusCode < http.StatusInternalServerError) || attempt == webSubAttempts-1 {
			return lastErr
		}

		if !waitForWebSubRetry(ctx, attempt) {
			return ctx.Err()
		}
	}

	return lastErr
}

func waitForWebSubRetry(ctx context.Context, attempt int) bool {
	timer := time.NewTimer(time.Duration(1<<attempt) * 500 * time.Millisecond)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func callbackToken(secret string) string {
	digest := sha256.Sum256([]byte(secret))

	return hex.EncodeToString(digest[:])
}

func verifyCallbackToken(secret, token string) bool {
	expected := callbackToken(secret)

	return hmac.Equal([]byte(expected), []byte(token))
}

func verifyWebSubSignature(secret, signature string, body []byte) bool {
	algorithm, hexadecimalDigest, found := strings.Cut(signature, "=")
	if !found || hexadecimalDigest == "" {
		return false
	}

	expectedDigest, err := hex.DecodeString(hexadecimalDigest)
	if err != nil {
		return false
	}

	var newHash func() hash.Hash

	switch strings.ToLower(algorithm) {
	case "sha1":
		newHash = sha1.New
	case "sha256":
		newHash = sha256.New
	case "sha384":
		newHash = sha512.New384
	case "sha512":
		newHash = sha512.New
	default:
		return false
	}

	mac := hmac.New(newHash, []byte(secret))

	mac.Write(body)

	return hmac.Equal(mac.Sum(nil), expectedDigest)
}
