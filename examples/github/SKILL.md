# GitHub

Use this tool to inspect a GitHub repository and the pull requests open
against it. Both operations are reads: the tool never writes to GitHub.

## When to use this tool

Reach for it when a task is grounded in what a specific repository or its pull
requests actually contain — whether a project is maintained, what a proposed
change does, or whether a change has been closed.

Do not use it to search GitHub as a whole. Both operations need a repository
you can already name; there is no search or repository listing here.

## Recommended workflows

Answering a question about a repository:

1. Use `get_repository` to confirm the repository exists and read its metadata.
2. Use `list_pull_requests` only when the question is about proposed changes.

A repository question is usually complete after a single `get_repository`
call. Widen to pull requests only when the task asks about them.

A compact repository-to-pull-requests sequence is:

`get_repository`
→ `list_pull_requests`

## Choosing between operations

- `get_repository` answers "what is this repository?" for one repository you
  have already named. `list_pull_requests` answers "what changes are
  proposed?" for that same repository. Do not use it to identify a repository.
- Both key on the same `owner` and `repo`, so resolve those once and reuse them
  across calls instead of re-deriving them per operation.
- `list_pull_requests` returns only open pull requests unless you pass `state`,
  so a question about closed or merged work needs that widened deliberately.

## Constraints

- Every call reaches api.github.com, and the tool declares auth, so a stored
  credential is required (`relay auth login github`). Without one the call
  fails with `AUTH_REQUIRED` before any request is sent.
- Both operations are reads. There is no write, merge, or comment operation.
- A repository the credential cannot see is reported by GitHub as a not-found
  result, not as a malformed request.

## Common mistakes

- Calling without a credential and reading `AUTH_REQUIRED` as a missing
  repository.
- Passing a repository's display name, or a full URL, as `owner` or `repo`;
  the API takes the URL path components.
- Treating the default pull request list as the complete history. It is open
  pull requests only.
- Assuming a short list is a complete list. See "Pagination" below.

## Pagination

This tool exposes no paging inputs, so `list_pull_requests` returns the
service's first page only, at GitHub's default page size. Treat a full-looking
page as partial: it may be truncated. When a complete list matters, narrow the
question (for example by `state`) rather than treating the first page as the
whole answer.

## Interpreting results

- The result is the GitHub object passed through unchanged. Read the fields the
  task asked about rather than echoing the whole object.
- A pull request's merge timestamp distinguishes merged work from work that
  was closed without being merged.
- Timestamps are ISO 8601 strings.
- GitHub does not answer a failed request with an HTTP 200, so a non-zero exit
  means the call genuinely failed.

## Manifest status

This example is the canonical manifest/skill pair and builds with `relay
build`, but it has not been exercised against the live GitHub API in this
repository. The endpoints match GitHub's documented REST API; treat them as
accurate to the documentation, not as live-verified.
