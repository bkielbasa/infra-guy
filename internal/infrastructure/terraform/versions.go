package terraform

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"infra-guy/internal/domain/project"
)

var FallbackVersions = []string{
	"1.11.4",
	"1.10.5",
	"1.9.8",
	"1.8.5",
	"1.7.5",
	"1.6.6",
	"1.5.7",
}

type VersionCatalog struct {
	URL    string
	Client *http.Client
	TTL    time.Duration

	mu        sync.Mutex
	cached    []string
	expiresAt time.Time
}

func (c *VersionCatalog) List(ctx context.Context) []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if len(c.cached) > 0 && now.Before(c.expiresAt) {
		return append([]string(nil), c.cached...)
	}
	versions, err := c.fetch(ctx)
	if err != nil || len(versions) == 0 {
		return append([]string(nil), FallbackVersions...)
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	c.cached = versions
	c.expiresAt = now.Add(ttl)
	return append([]string(nil), c.cached...)
}

func (c *VersionCatalog) fetch(ctx context.Context) ([]string, error) {
	u := c.URL
	if u == "" {
		u = "https://releases.hashicorp.com/terraform/index.json"
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	var payload struct {
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var parsed [][3]int
	var rawList []string
	for v := range payload.Versions {
		norm, err := project.ValidateTerraformVersion(v)
		if err != nil {
			continue
		}
		ver := parseSemver(norm)
		if ver[0] < 1 {
			continue
		}
		seen[norm] = true
		parsed = append(parsed, ver)
		rawList = append(rawList, norm)
	}
	if len(rawList) == 0 {
		return nil, nil
	}
	if !seen[project.DefaultTerraformVersion] {
		rawList = append(rawList, project.DefaultTerraformVersion)
	}
	sort.Slice(rawList, func(i, j int) bool {
		a, b := parseSemver(rawList[i]), parseSemver(rawList[j])
		if a[0] != b[0] {
			return a[0] > b[0]
		}
		if a[1] != b[1] {
			return a[1] > b[1]
		}
		return a[2] > b[2]
	})
	if len(rawList) > 30 {
		rawList = rawList[:30]
		hasDefault := false
		for _, v := range rawList {
			if v == project.DefaultTerraformVersion {
				hasDefault = true
				break
			}
		}
		if !hasDefault {
			rawList[len(rawList)-1] = project.DefaultTerraformVersion
		}
	}
	return rawList, nil
}

func parseSemver(v string) [3]int {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [3]int{}
	}
	a, _ := strconv.Atoi(parts[0])
	b, _ := strconv.Atoi(parts[1])
	c, _ := strconv.Atoi(parts[2])
	return [3]int{a, b, c}
}
