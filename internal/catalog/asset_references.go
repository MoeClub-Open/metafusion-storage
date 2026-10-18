package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MoeclubM/metafusion-storage/internal/store"
)

// AssetReferences uses only the dedicated internal credential. User tokens
// cannot confirm absence of private workspace assets for garbage collection.
func (c *Client) AssetReferences(secret string) store.CatalogAssetReferenceChecker {
	return func(ctx context.Context, refs []store.CatalogAssetReferenceCheck) ([]store.CatalogAssetReferenceCheck, error) {
		u, err := url.Parse(c.base)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimSpace(secret) == "" {
			return nil, errors.New("catalog_reference_confirmation_unavailable")
		}
		u.Path = "/internal/storage/asset-references"
		raw, err := json.Marshal(map[string]any{"references": refs})
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+secret)
		client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		res, err := client.Do(req)
		if err != nil {
			return nil, errors.New("catalog_reference_confirmation_unavailable")
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, errors.New("catalog_reference_confirmation_unavailable")
		}
		var out struct {
			References []store.CatalogAssetReferenceCheck `json:"references"`
		}
		dec := json.NewDecoder(io.LimitReader(res.Body, 2<<20))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&out); err != nil {
			return nil, errors.New("invalid_catalog_reference_confirmation")
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			return nil, errors.New("invalid_catalog_reference_confirmation")
		}
		return out.References, nil
	}
}
