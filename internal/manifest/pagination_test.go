package manifest

import (
	"strings"
	"testing"
)

// setPagination attaches a pagination block to the single operation in the
// valid REST fixture (spec §20).
func setPagination(doc *Document, p *Pagination) {
	doc.Tools[0].Request.Pagination = p
}

func TestValidateAcceptsPagination(t *testing.T) {
	tests := []struct {
		name string
		p    *Pagination
	}{
		{
			name: "link-header",
			p:    &Pagination{Style: PaginationStyleLinkHeader},
		},
		{
			name: "link-header with page size",
			p:    &Pagination{Style: PaginationStyleLinkHeader, LimitParam: "per_page", Limit: 100},
		},
		{
			name: "cursor with explicit query location",
			p:    &Pagination{Style: PaginationStyleCursor, CursorParam: "cursor", CursorIn: CursorInQuery, CursorField: "next_cursor"},
		},
		{
			name: "cursor with default location and has-more",
			p:    &Pagination{Style: PaginationStyleCursor, CursorParam: "cursor", CursorField: "meta.next", HasMoreField: "meta.has_more"},
		},
		{
			name: "cursor in body",
			p:    &Pagination{Style: PaginationStyleCursor, CursorParam: "cursor", CursorIn: CursorInBody, CursorField: "next_cursor"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc := mustParse(t, validManifest)
			setPagination(doc, test.p)
			if err := doc.Validate(); err != nil {
				t.Fatalf("expected valid pagination, got: %v", err)
			}
		})
	}
}

func TestValidateRejectsPagination(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Document)
		wantErr string
	}{
		{
			name:    "missing style",
			mutate:  func(d *Document) { setPagination(d, &Pagination{}) },
			wantErr: "request.pagination.style: required",
		},
		{
			name:    "unknown style",
			mutate:  func(d *Document) { setPagination(d, &Pagination{Style: "offset"}) },
			wantErr: `request.pagination.style: unknown style "offset"`,
		},
		{
			name: "cursor missing cursorParam",
			mutate: func(d *Document) {
				setPagination(d, &Pagination{Style: PaginationStyleCursor, CursorField: "next_cursor"})
			},
			wantErr: "request.pagination.cursorParam: required for cursor",
		},
		{
			name:    "cursor missing cursorField",
			mutate:  func(d *Document) { setPagination(d, &Pagination{Style: PaginationStyleCursor, CursorParam: "cursor"}) },
			wantErr: "request.pagination.cursorField: required for cursor",
		},
		{
			name: "link-header declaring cursorParam",
			mutate: func(d *Document) {
				setPagination(d, &Pagination{Style: PaginationStyleLinkHeader, CursorParam: "cursor"})
			},
			wantErr: "request.pagination.cursorParam: not allowed for link-header",
		},
		{
			name: "link-header declaring cursorIn",
			mutate: func(d *Document) {
				setPagination(d, &Pagination{Style: PaginationStyleLinkHeader, CursorIn: CursorInQuery})
			},
			wantErr: "request.pagination.cursorIn: not allowed for link-header",
		},
		{
			name: "link-header declaring cursorField",
			mutate: func(d *Document) {
				setPagination(d, &Pagination{Style: PaginationStyleLinkHeader, CursorField: "next_cursor"})
			},
			wantErr: "request.pagination.cursorField: not allowed for link-header",
		},
		{
			name: "link-header declaring hasMoreField",
			mutate: func(d *Document) {
				setPagination(d, &Pagination{Style: PaginationStyleLinkHeader, HasMoreField: "has_more"})
			},
			wantErr: "request.pagination.hasMoreField: not allowed for link-header",
		},
		{
			name: "bad cursorIn value",
			mutate: func(d *Document) {
				setPagination(d, &Pagination{Style: PaginationStyleCursor, CursorParam: "cursor", CursorField: "next_cursor", CursorIn: "header"})
			},
			wantErr: `request.pagination.cursorIn: "header" must be query or body`,
		},
		{
			name: "negative limit",
			mutate: func(d *Document) {
				setPagination(d, &Pagination{Style: PaginationStyleLinkHeader, LimitParam: "per_page", Limit: -1})
			},
			wantErr: "request.pagination.limit: -1 must not be negative",
		},
		{
			name: "limit without limitParam",
			mutate: func(d *Document) {
				setPagination(d, &Pagination{Style: PaginationStyleLinkHeader, Limit: 100})
			},
			wantErr: "request.pagination.limit: requires limitParam",
		},
		{
			name: "pagination on a non-GET method",
			mutate: func(d *Document) {
				d.Tools[0].Request.Method = "POST"
				setPagination(d, &Pagination{Style: PaginationStyleLinkHeader})
			},
			wantErr: "request.pagination: not allowed for method POST",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc := mustParse(t, validManifest)
			test.mutate(doc)
			err := doc.Validate()
			if err == nil {
				t.Fatalf("expected an error mentioning %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("expected error to mention %q, got: %v", test.wantErr, err)
			}
		})
	}
}

// Pagination is a REST transport strategy, so a GraphQL or local request that
// declares one is rejected rather than silently ignored (spec §20, §44, §46).
func TestValidateRejectsPaginationOnNonREST(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		wantErr string
	}{
		{name: "graphql", base: validGraphQLManifest, wantErr: "request.pagination: not allowed for graphql"},
		{name: "local", base: validLocalManifest, wantErr: "request.pagination: not allowed for local"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc := mustParse(t, test.base)
			setPagination(doc, &Pagination{Style: PaginationStyleCursor, CursorParam: "cursor", CursorField: "next_cursor"})
			err := doc.Validate()
			if err == nil {
				t.Fatalf("expected an error mentioning %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("expected error to mention %q, got: %v", test.wantErr, err)
			}
		})
	}
}

// TestValidatePaginationAggregatesProblems guards the all-at-once reporting
// property for the pagination rules (spec §59).
func TestValidatePaginationAggregatesProblems(t *testing.T) {
	doc := mustParse(t, validManifest)
	setPagination(doc, &Pagination{Style: PaginationStyleCursor, Limit: 5, LimitParam: ""})
	// cursorParam, cursorField, and limit-without-limitParam are three problems.

	err := doc.Validate()
	if err == nil {
		t.Fatal("expected an error")
	}
	validationErr, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if len(validationErr.Problems) != 3 {
		t.Fatalf("expected 3 problems, got %d: %v", len(validationErr.Problems), validationErr.Problems)
	}
}
