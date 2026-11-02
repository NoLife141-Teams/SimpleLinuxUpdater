// Package securityintel caches public lifecycle and exploitation metadata.
// It never sends inventory, versions, server names or credentials to a feed.
package securityintel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var Products = []string{"ubuntu", "debian", "python", "nodejs", "php", "postgresql", "redis", "nginx", "openssl", "openssh", "docker-engine", "containerd"}

const kevURL = "https://raw.githubusercontent.com/cisagov/kev-data/develop/known_exploited_vulnerabilities.json"
const maxFeedBytes = 8 << 20

type Release struct {
	Name         string `json:"name"`
	ReleaseDate  string `json:"releaseDate"`
	EOLFrom      string `json:"eolFrom"`
	EOASFrom     string `json:"eoasFrom"`
	EOESFrom     string `json:"eoesFrom"`
	IsEOL        *bool  `json:"isEol"`
	IsMaintained *bool  `json:"isMaintained"`
}
type Product struct {
	Name     string    `json:"name"`
	Label    string    `json:"label"`
	Releases []Release `json:"releases"`
}
type Exploitation struct {
	CVE    string `json:"cveID"`
	Added  string `json:"dateAdded"`
	Action string `json:"requiredAction"`
	Notes  string `json:"notes"`
}
type Feed struct {
	Source     string `json:"source"`
	ObservedAt string `json:"observed_at,omitempty"`
	State      string `json:"state"`
	LastError  string `json:"last_error,omitempty"`
}
type Snapshot struct {
	Products  map[string]Product      `json:"products"`
	Exploited map[string]Exploitation `json:"exploited"`
	Feeds     map[string]Feed         `json:"feeds"`
}
type cachedFeed struct {
	Source     string          `json:"source"`
	ObservedAt string          `json:"observed_at"`
	Data       json.RawMessage `json:"data"`
}
type Config struct {
	CacheDir, SeedDir string
	Now               func() time.Time
	Client            *http.Client
}
type Catalog struct {
	mu        sync.RWMutex
	refreshMu sync.Mutex
	start     sync.Once
	config    Config
	state     Snapshot
}

func New(config Config) *Catalog {
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Client == nil {
		config.Client = &http.Client{Timeout: 8 * time.Second}
	}
	c := &Catalog{config: config, state: Snapshot{Products: map[string]Product{}, Exploited: map[string]Exploitation{}, Feeds: map[string]Feed{}}}
	for key, source := range feedURLs() {
		c.state.Feeds[key] = Feed{Source: source, State: "unavailable"}
		var best cachedFeed
		for _, dir := range []string{config.SeedDir, config.CacheDir} {
			if dir == "" {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, key+".json"))
			if err != nil || len(data) > maxFeedBytes {
				continue
			}
			var f cachedFeed
			if json.Unmarshal(data, &f) != nil || f.Source != source || !validTime(f.ObservedAt, config.Now()) {
				continue
			}
			if f.ObservedAt > best.ObservedAt {
				best = f
			}
		}
		if best.ObservedAt != "" {
			_ = c.accept(key, best)
		}
	}
	return c
}
func feedURLs() map[string]string {
	out := map[string]string{"kev": kevURL}
	for _, product := range Products {
		out[product] = "https://endoflife.date/api/v1/products/" + product + "/"
	}
	return out
}
func validTime(value string, now time.Time) bool {
	at, err := time.Parse(time.RFC3339, value)
	return err == nil && !at.After(now)
}

// Snapshot is nonblocking with respect to network activity. Published maps and
// slices are immutable; callers must treat the returned snapshot as read-only.
func (c *Catalog) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := Snapshot{Products: map[string]Product{}, Exploited: c.state.Exploited, Feeds: map[string]Feed{}}
	for k, v := range c.state.Products {
		out.Products[k] = v
	}
	for k, v := range c.state.Feeds {
		at, err := time.Parse(time.RFC3339, v.ObservedAt)
		if err != nil {
			v.State = "unavailable"
		} else if c.config.Now().Sub(at) > 24*time.Hour || at.After(c.config.Now()) {
			v.State = "stale"
		} else {
			v.State = "fresh"
		}
		out.Feeds[k] = v
	}
	return out
}
func (c *Catalog) accept(key string, f cachedFeed) error {
	var product Product
	var exploited map[string]Exploitation
	if key == "kev" {
		var body struct {
			Vulnerabilities []Exploitation `json:"vulnerabilities"`
		}
		if err := json.Unmarshal(f.Data, &body); err != nil || body.Vulnerabilities == nil {
			return fmt.Errorf("invalid exploitation feed")
		}
		exploited = map[string]Exploitation{}
		for _, v := range body.Vulnerabilities {
			if v.CVE != "" {
				exploited[v.CVE] = v
			}
		}
	} else {
		var body struct {
			Result Product `json:"result"`
		}
		if err := json.Unmarshal(f.Data, &body); err != nil || body.Result.Name != key || len(body.Result.Releases) == 0 {
			return fmt.Errorf("invalid lifecycle feed")
		}
		product = body.Result
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if key == "kev" {
		c.state.Exploited = exploited
	} else {
		c.state.Products[key] = product
	}
	c.state.Feeds[key] = Feed{Source: f.Source, ObservedAt: f.ObservedAt, State: "fresh"}
	return nil
}
func (c *Catalog) Refresh(ctx context.Context) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	for key, source := range feedURLs() {
		if ctx.Err() != nil {
			return
		}
		if err := c.fetch(ctx, key, source); err != nil {
			c.mu.Lock()
			f := c.state.Feeds[key]
			f.LastError = "Public metadata refresh failed; retained previous observation"
			c.state.Feeds[key] = f
			c.mu.Unlock()
		}
	}
}
func (c *Catalog) fetch(ctx context.Context, key, source string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return err
	}
	resp, err := c.config.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("feed status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil || len(data) > maxFeedBytes {
		return fmt.Errorf("feed exceeds budget or cannot be read")
	}
	f := cachedFeed{Source: source, ObservedAt: c.config.Now().UTC().Format(time.RFC3339), Data: data}
	if err := c.accept(key, f); err != nil {
		return err
	}
	if c.config.CacheDir == "" {
		return nil
	}
	if err := os.MkdirAll(c.config.CacheDir, 0700); err != nil {
		return err
	}
	encoded, err := json.Marshal(f)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(c.config.CacheDir, ".feed-")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err = temp.Write(encoded); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(c.config.CacheDir, key+".json"))
}
func (c *Catalog) Start(ctx context.Context) {
	c.start.Do(func() {
		go func() {
			timer := time.NewTicker(6 * time.Hour)
			defer timer.Stop()
			for {
				c.Refresh(ctx)
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
				}
			}
		}()
	})
}
