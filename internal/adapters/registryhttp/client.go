package registryhttp

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

	"github.com/JustIkra/umir-feedback/internal/application"
)

const (
	registryPath = "/internal/auth/v1/registry"
	pageLimit    = 1000
	maxBodyBytes = 4 << 20
)

type tombstone struct {
	RestaurantID     string `json:"restaurantID"`
	OrgID            string `json:"orgID"`
	LifecycleVersion int64  `json:"lifecycleVersion"`
}

type page struct {
	RegistryRevision int64             `json:"registryRevision"`
	Complete         bool              `json:"complete"`
	NextAfter        string            `json:"nextAfter,omitempty"`
	Entries          []json.RawMessage `json:"entries"`
	Tombstones       []tombstone       `json:"tombstones"`
}

type Client struct {
	base string
	key  string
	http *http.Client
}

func New(baseURL, serviceKey string, client *http.Client) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" || strings.TrimSpace(serviceKey) == "" || client == nil {
		return nil, errors.New("invalid registry client configuration")
	}
	safeClient := &http.Client{
		Transport:     client.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Jar:           client.Jar,
		Timeout:       client.Timeout,
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), key: serviceKey, http: safeClient}, nil
}

func (c *Client) Changes(ctx context.Context, since int64, after string) (result application.RegistryPage, returnErr error) {
	if since < 0 || len(after) > 256 {
		return application.RegistryPage{}, errors.New("invalid registry cursor")
	}
	endpoint, err := url.Parse(c.base + registryPath)
	if err != nil {
		return application.RegistryPage{}, errors.New("invalid registry endpoint")
	}
	query := endpoint.Query()
	query.Set("since", strconv.FormatInt(since, 10))
	query.Set("limit", strconv.Itoa(pageLimit))
	if after != "" {
		query.Set("after", after)
	}
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return application.RegistryPage{}, errors.New("create registry request")
	}
	request.Header.Set("X-Umir-Service-Key", c.key)
	response, err := c.http.Do(request)
	if err != nil {
		return application.RegistryPage{}, errors.New("request Auth registry")
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			returnErr = errors.Join(returnErr, errors.New("close Auth registry response"))
		}
	}()
	if response.StatusCode != http.StatusOK {
		return application.RegistryPage{}, fmt.Errorf("auth registry returned status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		return application.RegistryPage{}, errors.New("read Auth registry response")
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	var responsePage page
	if err := decoder.Decode(&responsePage); err != nil {
		return application.RegistryPage{}, errors.New("decode Auth registry response")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return application.RegistryPage{}, errors.New("decode Auth registry response")
	}
	if responsePage.RegistryRevision < 1 || responsePage.Entries == nil || responsePage.Tombstones == nil || len(responsePage.Entries)+len(responsePage.Tombstones) > pageLimit || (!responsePage.Complete && (responsePage.NextAfter == "" || len(responsePage.NextAfter) > 256)) || (responsePage.Complete && responsePage.NextAfter != "") {
		return application.RegistryPage{}, errors.New("invalid Auth registry page")
	}
	page := application.RegistryPage{Revision: responsePage.RegistryRevision, Complete: responsePage.Complete, NextAfter: responsePage.NextAfter}
	for _, tombstone := range responsePage.Tombstones {
		if !uuidV4(tombstone.RestaurantID) || !uuidV4(tombstone.OrgID) || tombstone.LifecycleVersion < 1 {
			return application.RegistryPage{}, errors.New("invalid Auth registry tombstone")
		}
		page.Tombstones = append(page.Tombstones, application.RegistryTombstone{RestaurantID: tombstone.RestaurantID, OrgID: tombstone.OrgID, LifecycleVersion: tombstone.LifecycleVersion})
	}
	return page, nil
}

func uuidV4(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' || !strings.ContainsRune("89ab", rune(value[19])) {
		return false
	}
	for index, r := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'f' {
			continue
		}
		return false
	}
	return true
}
