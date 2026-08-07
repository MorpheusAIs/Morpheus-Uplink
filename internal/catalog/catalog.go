// Package catalog maps human model names to blockchain model IDs using the
// active.mor.org catalog JSON (the same source the hosted APIGW uses).
package catalog

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Model struct {
	Name string   `json:"Name"`
	ID   string   `json:"Id"`
	Tags []string `json:"Tags"`
}

type activeModelsFile struct {
	Models []Model `json:"models"`
}

type Catalog struct {
	url    string
	client *http.Client

	mu        sync.Mutex
	models    []Model
	byName    map[string]string // lowercase name -> id
	byID      map[string]string // lowercase id -> display name
	fetchedAt time.Time
	ttl       time.Duration
}

func New(url string) *Catalog {
	return &Catalog{
		url:    url,
		client: &http.Client{Timeout: 15 * time.Second},
		ttl:    5 * time.Minute,
	}
}

func (c *Catalog) refresh() error {
	resp, err := c.client.Get(c.url)
	if err != nil {
		return fmt.Errorf("fetch model catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch model catalog: status %d", resp.StatusCode)
	}
	var file activeModelsFile
	if err := json.NewDecoder(resp.Body).Decode(&file); err != nil {
		return fmt.Errorf("parse model catalog: %w", err)
	}
	byName := make(map[string]string, len(file.Models))
	byID := make(map[string]string, len(file.Models))
	for _, m := range file.Models {
		byName[strings.ToLower(m.Name)] = m.ID
		byID[strings.ToLower(m.ID)] = m.Name
	}
	c.models = file.Models
	c.byName = byName
	c.byID = byID
	c.fetchedAt = time.Now()
	return nil
}

func (c *Catalog) ensureFresh() error {
	if time.Since(c.fetchedAt) < c.ttl && c.models != nil {
		return nil
	}
	err := c.refresh()
	// Serve stale data over failing hard if we have any.
	if err != nil && c.models != nil {
		return nil
	}
	return err
}

// Resolve maps a model name (or an already-resolved 0x id) to a blockchain
// model ID.
func (c *Catalog) Resolve(nameOrID string) (string, error) {
	if strings.HasPrefix(nameOrID, "0x") {
		return nameOrID, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureFresh(); err != nil {
		return "", err
	}
	if id, ok := c.byName[strings.ToLower(nameOrID)]; ok {
		return id, nil
	}
	return "", fmt.Errorf("model %q not found in catalog", nameOrID)
}

func (c *Catalog) List() ([]Model, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureFresh(); err != nil {
		return nil, err
	}
	out := make([]Model, len(c.models))
	copy(out, c.models)
	return out, nil
}

// NameByID returns the catalog display name for a blockchain model id.
// Empty string if unknown.
func (c *Catalog) NameByID(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureFresh(); err != nil {
		return ""
	}
	return c.byID[strings.ToLower(id)]
}
