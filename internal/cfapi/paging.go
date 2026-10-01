package cfapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

const (
	// Some endpoints refuse a larger page than this (zones allow 50).
	defaultPageSize = 50
	// A listing this long is a server that never ends, not a zone.
	maxPages = 1000
)

// errListingChanged marks a listing whose pages do not add up: the set
// changed while it was read, or the server left items out.
var errListingChanged = errors.New("listing changed while it was read")

// list reads every page of the collection at path, then calls each with every
// item in order. Nothing is passed to each unless the whole listing was read
// and adds up: a listing that failed half way, or that cannot be shown to be
// complete, is an error and never a shorter list. The caller retries later.
//
// The page size is the per_page of query, or 50. How many pages there are is
// taken from result_info: total_pages, else total_count over the page size.
// With neither, a page that is not full is the only page, and a full one is an
// error. A page that result_info numbers must be the page asked for, every
// page of a listing of more than one page must hold items, total_count and
// total_pages must not change between pages, and the items read must add up
// to total_count.
func (c *Client) list(ctx context.Context, path string, query url.Values, each func(json.RawMessage) error) error {
	size, err := requestedPageSize(query)
	if err != nil {
		return err
	}

	var (
		items      []json.RawMessage
		first      *resultInfo
		totalPages = 1
	)
	for page := 1; page <= totalPages; page++ {
		env, err := c.roundTrip(ctx, http.MethodGet, path, pageQuery(query, page, size), nil)
		if err != nil {
			return fmt.Errorf("listing %s, page %d: %w", path, page, err)
		}
		batch, err := pageItems(env)
		if err != nil {
			return fmt.Errorf("listing %s, page %d: %w", path, page, err)
		}
		if err := env.ResultInfo.isPage(page); err != nil {
			return fmt.Errorf("listing %s, page %d: %w", path, page, err)
		}
		items = append(items, batch...)

		if page > 1 {
			if err := sameListing(first, env.ResultInfo); err != nil {
				return fmt.Errorf("listing %s, page %d: %w", path, page, err)
			}
		} else {
			first = env.ResultInfo
			if first != nil && first.PerPage > 0 {
				size = first.PerPage // what the server really uses
			}
			if totalPages, err = pageCount(first, len(batch), size); err != nil {
				return fmt.Errorf("listing %s: %w", path, err)
			}
		}
		if len(batch) == 0 && totalPages > 1 {
			// Whatever was to fill it moved, or the server left it out.
			return fmt.Errorf("listing %s: %w: page %d is empty, %d pages expected", path, errListingChanged, page, totalPages)
		}
	}

	if first != nil && first.TotalCount != nil && len(items) != *first.TotalCount {
		return fmt.Errorf("listing %s: %w: read %d items, the server counts %d",
			path, errListingChanged, len(items), *first.TotalCount)
	}
	for _, item := range items {
		if err := each(item); err != nil {
			return err
		}
	}
	return nil
}

func requestedPageSize(query url.Values) (int, error) {
	v := query.Get("per_page")
	if v == "" {
		return defaultPageSize, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, errors.New("per_page must be a positive number")
	}
	return n, nil
}

func pageQuery(query url.Values, page, size int) url.Values {
	q := make(url.Values, len(query)+2)
	for k, v := range query {
		q[k] = append([]string(nil), v...)
	}
	q.Set("page", strconv.Itoa(page))
	q.Set("per_page", strconv.Itoa(size))
	return q
}

// pageItems returns the items of one page. A null or missing result is an
// empty page only where result_info says that nothing was returned.
func pageItems(env *envelope) ([]json.RawMessage, error) {
	if isNull(env.Result) {
		if env.ResultInfo.reportsNone() {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: no result", errUnexpected)
	}
	var batch []json.RawMessage
	if err := json.Unmarshal(env.Result, &batch); err != nil {
		return nil, fmt.Errorf("%w: result is not a list", errUnexpected)
	}
	return batch, nil
}

// reportsNone says whether result_info states that the page has no items.
func (i *resultInfo) reportsNone() bool {
	return i != nil && (i.Count != nil && *i.Count == 0 || i.TotalCount != nil && *i.TotalCount == 0)
}

// isPage checks that result_info, when it numbers the page, numbers it as
// the page asked for: a server that answers another page, the first one again
// for instance, would make items count twice and others go missing.
func (i *resultInfo) isPage(asked int) error {
	if i != nil && i.Page != nil && *i.Page != asked {
		return fmt.Errorf("%w: asked for page %d, got page %d", errUnexpected, asked, *i.Page)
	}
	return nil
}

// pageCount says how many pages the listing has, from what the first page
// told.
func pageCount(info *resultInfo, items, size int) (int, error) {
	var pages int
	switch {
	case info != nil && info.TotalPages != nil:
		pages = *info.TotalPages
		if pages <= 0 && items > 0 {
			return 0, fmt.Errorf("%w: total_pages is %d, but the page holds %d items", errUnexpected, pages, items)
		}
	case info != nil && info.TotalCount != nil:
		pages = (*info.TotalCount + size - 1) / size
	case items < size:
		pages = 1
	default:
		return 0, fmt.Errorf("%w: a full page of %d items and no total to tell if more follow", errUnexpected, items)
	}
	if pages > maxPages {
		return 0, fmt.Errorf("%d pages is more than the limit of %d", pages, maxPages)
	}
	return max(pages, 1), nil
}

// sameListing checks that a later page reports the same totals as the first.
func sameListing(first, next *resultInfo) error {
	var a, b resultInfo // absent result_info is the same as one without counts
	if first != nil {
		a = *first
	}
	if next != nil {
		b = *next
	}
	if !samePtr(a.TotalCount, b.TotalCount) {
		return fmt.Errorf("%w: total_count differs between pages", errListingChanged)
	}
	if !samePtr(a.TotalPages, b.TotalPages) {
		return fmt.Errorf("%w: total_pages differs between pages", errListingChanged)
	}
	return nil
}

func samePtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
