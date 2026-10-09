package store

import "time"

// Subscription is one delayed feed configuration. A show's regular
// (free) feed is SourceURL; PremiumSourceURL is the publisher's
// premium/bonus feed melded into the same delayed feed, nil when there
// isn't one.
type Subscription struct {
	ID                  int64
	Token               string
	SourceURL           string
	PremiumSourceURL    *string
	TitleOverride       *string
	CadenceDays         int
	CadenceMode         string // schedule.ModeFixed or schedule.ModeOriginal
	ReleaseTime         string
	Timezone            string
	StartAt             time.Time
	SeedCount           int
	EpisodesPerSlot     int
	ShiftSeconds        int
	PausedAt            *time.Time
	HiddenAt            *time.Time
	MaxFeedItems        *int
	ChannelJSON         string
	ETag                *string
	LastModified        *string
	PremiumETag         *string
	PremiumLastModified *string
	LastFetchedAt       *time.Time
	LastFetchStatus     *string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Episode is one item in a subscription's frozen ingest order.
type Episode struct {
	ID             int64
	SubscriptionID int64
	GUID           string
	// SourceRole is which of the show's feeds this came from:
	// schedule.RolePrimary or schedule.RolePremium.
	SourceRole      string
	Position        int
	ScheduledAt     time.Time
	Locked          bool
	Excluded        bool
	OriginalPubDate *time.Time
	Title           string
	Description     string
	Link            string
	EnclosureURL    string
	EnclosureType   string
	EnclosureLength *int64
	Duration        string
	EpisodeNumber   *int
	Season          *int
	EpisodeType     string
	Explicit        *bool
	ImageURL        string
	FirstSeenAt     time.Time
	MissingSince    *time.Time
}

// ChannelMeta is the cached show-level metadata rendered into the feed
// without touching the source on every request.
type ChannelMeta struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Link        string   `json:"link"`
	Language    string   `json:"language"`
	Author      string   `json:"author"`
	ImageURL    string   `json:"image_url"`
	Explicit    bool     `json:"explicit"`
	ItunesType  string   `json:"itunes_type"`
	Categories  []string `json:"categories"`
	Owner       string   `json:"owner"`
	OwnerEmail  string   `json:"owner_email"`
}
