package main

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"unicode"

	"github.com/goccy/go-yaml"
)

const (
	ConfigPath           = "config.yml"
	DefaultListenAddress = ":5151"
)

type Config struct {
	Server   ServerConfig        `yaml:"server"`
	YouTube  YouTubeConfig       `yaml:"youtube"`
	Groups   []GroupConfig       `yaml:"groups"`
	channels map[string]struct{} `yaml:"-"`
}

type ServerConfig struct {
	Listen    string `yaml:"listen"`
	PublicURL string `yaml:"public-url"`
}

type YouTubeConfig struct {
	APIKey        string `yaml:"api-key"`
	IncludeShorts bool   `yaml:"include-shorts"`
	Subscribe     bool   `yaml:"subscribe"`
}

type GroupConfig struct {
	Name     string   `yaml:"name"`
	Channels []string `yaml:"channels"`
	Slug     string   `yaml:"-"`
}

func LoadConfig() (*Config, error) {
	contents, err := os.ReadFile(ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	expanded := os.ExpandEnv(string(contents))

	var config Config

	decoder := yaml.NewDecoder(bytes.NewReader([]byte(expanded)), yaml.DisallowUnknownField())

	err = decoder.Decode(&config)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	err = config.prepare()
	if err != nil {
		return nil, err
	}

	return &config, nil
}

func (c *Config) prepare() error {
	if c.Server.Listen == "" {
		c.Server.Listen = DefaultListenAddress
	}

	if c.Server.PublicURL == "" {
		return fmt.Errorf("server.public-url is required")
	}

	publicURL, err := url.Parse(c.Server.PublicURL)
	if err != nil {
		return fmt.Errorf("parse server.public-url: %w", err)
	}

	if publicURL.Scheme != "http" && publicURL.Scheme != "https" {
		return fmt.Errorf("server.public-url must use http or https")
	}

	if publicURL.Host == "" {
		return fmt.Errorf("server.public-url must include a host")
	}

	c.Server.PublicURL = strings.TrimRight(c.Server.PublicURL, "/")

	if c.YouTube.APIKey == "" {
		return fmt.Errorf("youtube.api-key is required")
	}

	if len(c.Groups) == 0 {
		return fmt.Errorf("at least one group is required")
	}

	c.channels = make(map[string]struct{})

	groupNames := make(map[string]struct{})
	groupSlugs := make(map[string]struct{})

	for index := range c.Groups {
		group := &c.Groups[index]

		group.Name = strings.TrimSpace(group.Name)
		if group.Name == "" {
			return fmt.Errorf("group %d has no name", index+1)
		}

		if _, exists := groupNames[group.Name]; exists {
			return fmt.Errorf("duplicate group name %q", group.Name)
		}

		groupNames[group.Name] = struct{}{}

		group.Slug = slugify(group.Name)
		if group.Slug == "" {
			return fmt.Errorf("group %q does not produce a usable slug", group.Name)
		}

		if _, exists := groupSlugs[group.Slug]; exists {
			return fmt.Errorf("duplicate group slug %q", group.Slug)
		}

		groupSlugs[group.Slug] = struct{}{}

		if len(group.Channels) == 0 {
			return fmt.Errorf("group %q has no channels", group.Name)
		}

		seen := make(map[string]struct{})

		for channelIndex := range group.Channels {
			channelID := strings.TrimSpace(group.Channels[channelIndex])
			if channelID == "" {
				return fmt.Errorf("group %q contains an empty channel id", group.Name)
			}

			if !strings.HasPrefix(channelID, "UC") {
				return fmt.Errorf("group %q contains invalid channel id %q", group.Name, channelID)
			}

			if _, exists := seen[channelID]; exists {
				return fmt.Errorf("group %q contains channel %q more than once", group.Name, channelID)
			}

			seen[channelID] = struct{}{}
			c.channels[channelID] = struct{}{}
			group.Channels[channelIndex] = channelID
		}
	}

	return nil
}

func (c *Config) ChannelIDs() []string {
	channelIDs := make([]string, 0, len(c.channels))

	for channelID := range c.channels {
		channelIDs = append(channelIDs, channelID)
	}

	return channelIDs
}

func (c *Config) HasChannel(channelID string) bool {
	_, exists := c.channels[channelID]

	return exists
}

func (c *Config) Group(slug string) *GroupConfig {
	for index := range c.Groups {
		if c.Groups[index].Slug == slug {
			return &c.Groups[index]
		}
	}

	return nil
}

func (c *Config) GroupNames(channelID string) []string {
	groups := make([]string, 0, 2)

	for index := range c.Groups {
		group := &c.Groups[index]

		if slices.Contains(group.Channels, channelID) {
			groups = append(groups, group.Name)
		}
	}

	return groups
}

func slugify(value string) string {
	var (
		builder     strings.Builder
		pendingDash bool
	)

	for _, char := range strings.ToLower(value) {
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			if pendingDash && builder.Len() > 0 {
				builder.WriteByte('-')
			}

			builder.WriteRune(char)

			pendingDash = false

			continue
		}

		pendingDash = true
	}

	return builder.String()
}
