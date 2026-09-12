# dashboard

Use this tool to read reports from a web dashboard that has no API of its own.
Every operation runs as the logged-in account, so it sees exactly what that
account sees in a browser.

## Before you start

The tool needs a session. If a call fails with `AUTH_REQUIRED`, the session is
missing or expired and a human has to establish it — there is no way for you to
log in on their behalf, because the credential lives in the Keychain and only
that human can supply it.

## Recommended workflows

1. Start with the listing operation to see which reports exist.
2. Fetch one report by the id the listing returned.

## Constraints

Expect HTML, not JSON. Put the data to use for its content rather than trying to
parse it into a structure; if a field you need is not visible on the page, it is
not available through this tool.

Sessions expire on the dashboard's schedule, not Relay's. A call that worked an
hour ago can fail with `AUTH_REQUIRED`, and that is normal rather than a bug.

## Common mistakes

- Guessing a report id instead of listing first.
- Treating an `AUTH_REQUIRED` failure as a permanent error — it is fixable, by a human.
