# Slack

Use this tool to read and post in a Slack workspace: discover conversations,
read a conversation's recent history, post a message, and resolve a user id to
a person.

## When to use this tool

Reach for Slack when a task depends on what a team has actually said in a
workspace — finding a decision, summarizing a channel, or answering "who owns
this?" from recent messages.

Do not use it as a generic notification channel. Posting into a shared channel
is a visible action other people see, so only post when the task explicitly
calls for sending a message.

## Recommended workflows

Posting an update:

1. Use `auth_test` to confirm which workspace and identity the credential belongs to.
2. Use `list_conversations` to find the target conversation id.
3. Use `post_message` with that id and the message text.

Reading a recent conversation:

1. Use `list_conversations` when you only have a channel name.
2. Use `conversations_history` to pull the messages.
3. Use `get_user` to resolve the authors you intend to name.

A compact sequence for reading is:

`list_conversations`
→ `conversations_history`
→ `get_user`

## Choosing between operations

- `list_conversations` discovers which conversations exist. `conversations_history` reads the messages inside one conversation you have already identified. Do not reach for history until you hold a conversation id.
- `auth_test` is a cheap preflight. When several calls will follow, run it first so a wrong or expired credential fails immediately instead of midway through a workflow.
- `get_user` resolves one id at a time. Prefer resolving only the authors you actually need rather than every distinct id in a large history.

## Constraints

- Slack methods take conversation ids, not names. A name like a project channel must be resolved to an id through `list_conversations` before reading or posting.
- Posting is a write. The runtime does not retry POST calls, so a transport failure can leave you unsure whether the message landed. Re-read the conversation with `conversations_history` before posting again.
- Pagination is cursor-based and cursors expire. Do not store one for later; use it within the same workflow.

## Common mistakes

- Treating a successful HTTP call as a successful Slack method. See "Interpreting results" below.
- Passing a channel name where a channel id is required, which fails at the Slack method rather than at the CLI.
- Reading only the first page of a busy conversation and reporting it as the whole story.
- Posting a summary into the very channel it was derived from without being asked to.

## Pagination

Slack returns pages with an opaque cursor in the response metadata. Pass that
value back as `cursor` to fetch the next page, and stop when the response no
longer carries one. Keep `limit` modest and page deliberately; a large limit
still returns a cursor when more messages remain.

## Interpreting results

- Check the `ok` field first. Slack answers most methods with HTTP 200 even when the call failed, putting the real outcome in `ok` and a machine-readable reason in an `error` field. The transport-level result says nothing about whether the Slack method succeeded.
- Message timestamps (`ts`) are strings and double as message ids. Compare them numerically after parsing, not as text.
- Message text is Slack mrkdwn, not Markdown; formatting such as emphasis and links differs, so quote it rather than silently reformatting it.
- User objects are larger than they look. Resolve the display name and id you need, and avoid echoing profile fields the task did not ask for.

## Manifest status

This example is modeled on the Slack Web API and builds with `relay build`, but
it has not been exercised against a live workspace in this repository. Treat the
endpoints as accurate to Slack's documented methods, not as verified. Slack's
`ok: false` convention is handled by the agent reading the result, not by the
REST executor, which maps HTTP status only.
