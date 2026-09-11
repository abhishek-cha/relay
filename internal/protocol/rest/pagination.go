package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"relay/internal/protocol"
)

// Pagination strategy and cursor-location vocabularies understood by the
// executor. They mirror the manifest constants in internal/manifest; the
// executor keeps its own copy so the protocol layer does not depend on the
// manifest package (spec §19).
const (
	paginationStyleLinkHeader = "link-header"
	paginationStyleCursor     = "cursor"
	cursorInQuery             = "query"
	cursorInBody              = "body"
)

// executePaginated walks a paginated REST operation and returns one response
// whose body keeps the shape of a single page (spec §20).
//
// The walk stops when the declared strategy finds no next page. It is also
// bounded by two hard caps — a maximum page count and a maximum total body
// size — so a backend that always offers another page, or one that returns
// oversized pages, cannot spin forever or exhaust memory. When a cap stops the
// walk while a next page still exists, the response is marked Truncated rather
// than passed off as the whole collection.
//
// A failure on any page is surfaced as the operation's error; the caller sees
// the structured §26 error exactly as it would for a single request.
func (e *Executor) executePaginated(ctx context.Context, method string, start *url.URL, headers http.Header, bodyBytes []byte, pag *protocol.Pagination) (protocol.Response, error) {
	maxPages := e.effectiveMaxPages()
	maxBytes := e.effectiveMaxTotalBytes()

	// The page-size parameter is a property of the first request; a link-header
	// backend carries it forward in the next link it returns.
	nextURL := start.String()
	if pag.LimitParam != "" && pag.Limit > 0 {
		nextURL = withQueryParam(start, pag.LimitParam, strconv.Itoa(pag.Limit)).String()
	}
	nextBody := bodyBytes

	var (
		aggregate  any
		headersOut map[string][]string
		status     int
		haveBody   bool
		totalBytes int
		pages      int
		truncated  bool
	)

	for {
		resp, size, err := e.executeWithRetry(ctx, method, nextURL, headers, nextBody)
		if err != nil {
			return protocol.Response{}, err
		}
		totalBytes += size
		pages++

		if !haveBody {
			aggregate, headersOut, status, haveBody = resp.Body, resp.Headers, resp.Status, true
		} else {
			aggregate = mergeBodies(aggregate, resp.Body)
		}

		following, body, more := nextPage(pag, resp, nextURL)
		if !more {
			break
		}
		if pages >= maxPages || int64(totalBytes) >= maxBytes {
			truncated = true
			break
		}
		nextURL, nextBody = following, body
	}

	return protocol.Response{
		Status:    status,
		Headers:   headersOut,
		Body:      aggregate,
		Pages:     pages,
		Truncated: truncated,
	}, nil
}

// nextPage decides whether another page follows and builds its request. It
// returns the next URL, an optional next request body, and whether a next page
// exists at all.
func nextPage(pag *protocol.Pagination, resp protocol.Response, currentURL string) (string, []byte, bool) {
	switch pag.Style {
	case paginationStyleLinkHeader:
		target, ok := nextLink(resp.Headers)
		if !ok {
			return "", nil, false
		}
		return resolveNextURL(currentURL, target), nil, true

	case paginationStyleCursor:
		// An optional has-more field can end the walk before the cursor does,
		// which is the only way some APIs signal that the last page arrived.
		if pag.HasMoreField != "" {
			if value, ok := lookupPath(resp.Body, pag.HasMoreField); ok && !truthy(value) {
				return "", nil, false
			}
		}
		cursor, ok := lookupCursor(resp.Body, pag.CursorField)
		if !ok || cursor == "" {
			return "", nil, false
		}
		switch pag.CursorIn {
		case cursorInBody:
			encoded, err := json.Marshal(map[string]any{pag.CursorParam: cursor})
			if err != nil {
				return "", nil, false
			}
			return currentURL, encoded, true
		case "", cursorInQuery:
			parsed, err := url.Parse(currentURL)
			if err != nil {
				return "", nil, false
			}
			return withQueryParam(parsed, pag.CursorParam, cursor).String(), nil, true
		}
		return "", nil, false
	}

	return "", nil, false
}

// mergeBodies combines two page bodies into one value that keeps the shape of a
// single-page result (spec §20): two JSON arrays concatenate, two JSON objects
// merge key by key (array members concatenate, a scalar from the later page
// wins), and any other pairing keeps the later page. An array-returning API and
// an envelope-returning one therefore both aggregate without changing the
// result's JSON type.
func mergeBodies(a, b any) any {
	switch av := a.(type) {
	case []any:
		if bv, ok := b.([]any); ok {
			return append(av, bv...)
		}
	case map[string]any:
		if bv, ok := b.(map[string]any); ok {
			for key, value := range bv {
				if existing, ok := av[key]; ok {
					av[key] = mergeBodies(existing, value)
				} else {
					av[key] = value
				}
			}
			return av
		}
	}
	return b
}

// lookupPath resolves a dotted path within a decoded JSON value. Object keys
// are looked up by name and array positions by an integer index.
func lookupPath(root any, path string) (any, bool) {
	if path == "" {
		return nil, false
	}
	current := root
	for _, segment := range strings.Split(path, ".") {
		switch node := current.(type) {
		case map[string]any:
			value, ok := node[segment]
			if !ok {
				return nil, false
			}
			current = value
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(node) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

// lookupCursor reads the cursor at field and renders it as a request value. A
// missing field, JSON null, or empty string all mean the walk is finished.
func lookupCursor(body any, field string) (string, bool) {
	value, ok := lookupPath(body, field)
	if !ok || value == nil {
		return "", false
	}
	switch v := value.(type) {
	case string:
		return v, v != ""
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(v), true
	default:
		return "", false
	}
}

// truthy reports whether a has-more value means "more pages remain". JSON has no
// boolean scalar kind beyond bool, so numbers and strings are read on their
// conventional falsy values as well.
func truthy(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v != "" && v != "0" && !strings.EqualFold(v, "false")
	case nil:
		return false
	default:
		return true
	}
}

// withQueryParam returns a copy of u with name set to value.
func withQueryParam(u *url.URL, name, value string) *url.URL {
	clone := *u
	query := clone.Query()
	query.Set(name, value)
	clone.RawQuery = query.Encode()
	return &clone
}

// resolveNextURL resolves a Link-header target, which may be absolute or
// relative, against the URL it came from.
func resolveNextURL(current, target string) string {
	base, err := url.Parse(current)
	if err != nil {
		return target
	}
	ref, err := url.Parse(target)
	if err != nil {
		return target
	}
	return base.ResolveReference(ref).String()
}

// linkEntry is one `<target>; rel="..."` value from a Link header.
type linkEntry struct {
	target  string
	relNext bool
}

// nextLink returns the target of the first `rel="next"` entry across every
// Link header value (RFC 8288).
func nextLink(headers map[string][]string) (string, bool) {
	for name, values := range headers {
		if !strings.EqualFold(name, "Link") {
			continue
		}
		for _, value := range values {
			for _, entry := range parseLinkHeader(value) {
				if entry.relNext {
					return entry.target, true
				}
			}
		}
	}
	return "", false
}

// parseLinkHeader splits an RFC 8288 Link header value into entries. Commas
// separate entries, but a comma inside a URL lives within its angle brackets,
// so only a comma outside the brackets starts a new entry.
func parseLinkHeader(value string) []linkEntry {
	var entries []linkEntry
	i := 0
	for i < len(value) {
		for i < len(value) && (value[i] == ' ' || value[i] == '\t' || value[i] == ',') {
			i++
		}
		if i >= len(value) || value[i] != '<' {
			break
		}
		end := strings.IndexByte(value[i:], '>')
		if end < 0 {
			break
		}
		target := value[i+1 : i+end]
		i += end + 1

		start := i
		depth := 0
		inQuotes := false
		for i < len(value) {
			c := value[i]
			if inQuotes {
				if c == '"' {
					inQuotes = false
				}
				i++
				continue
			}
			switch {
			case c == '"':
				inQuotes = true
			case c == '<':
				depth++
			case c == '>':
				depth--
			case c == ',' && depth == 0:
				// End of this entry; leave i on the comma for the outer loop.
				goto entryDone
			}
			i++
		}
	entryDone:
		params := value[start:i]
		if i < len(value) && value[i] == ',' {
			i++
		}
		entries = append(entries, linkEntry{target: target, relNext: linkRelNext(params)})
	}
	return entries
}

// linkRelNext reports whether the parameters of a Link entry contain a rel
// token equal to "next". The rel parameter may list several space-separated
// relation types.
func linkRelNext(params string) bool {
	for _, param := range strings.Split(params, ";") {
		key, value, found := strings.Cut(param, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(key), "rel") {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"`)
		for _, rel := range strings.Fields(value) {
			if strings.EqualFold(rel, "next") {
				return true
			}
		}
	}
	return false
}
