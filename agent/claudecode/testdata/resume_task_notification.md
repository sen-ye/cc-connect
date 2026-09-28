This event sequence was captured with Claude Code 2.1.283 while resuming an
isolated transcript with an undelivered stopped-background-task notification.
The new user input was the local `/context` command; no model generation ran.
UUIDs and the context report were replaced with test values. Routing fields,
zero-turn success semantics, and event ordering are retained.

The notification is terminal for an autonomous prompt, not for the client's
queued input. A zero-turn success can also be a valid client response, so
neither empty text nor `num_turns == 0` is a completion discriminator.
