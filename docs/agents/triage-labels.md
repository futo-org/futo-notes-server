# Triage Labels

The skills speak in terms of five canonical triage roles. This file maps those
roles to the actual label strings in this repo's issue tracker.

**Only create labels a human asked for.** Where a role has no label below, say
the thing in the issue body instead of inventing a label for it.

| Role in mattpocock/skills | Label in our tracker | Meaning                                  |
| ------------------------- | -------------------- | ---------------------------------------- |
| `needs-triage`            | *(none)*             | Maintainer needs to evaluate this issue  |
| `needs-info`              | *(none)*             | Waiting on reporter for more information |
| `ready-for-agent`         | `ready-for-agent`    | Fully specified, ready for an AFK agent  |
| `ready-for-human`         | *(none)*             | Requires human implementation            |
| `wontfix`                 | `Deferred`           | Not being actioned now                   |

`ready-for-agent` is the one a skill should apply on its own, and only when the
issue really is fully specified. Everything else in the table is a human's call.

For the three roles with no label: write the state into the issue body. An issue
nobody has prioritised gets a closing section saying so — that reads better than
a `needs-triage` label would, and it has room for what is actually undecided.

## The full label set

These are the labels that exist. Applying anything else creates a new label, so
don't.

| Label             | Use for                                          |
| ----------------- | ------------------------------------------------ |
| `bug`             | Something is broken                              |
| `crash`           | A crash specifically                             |
| `ci`              | The build, the test suite, the pipeline          |
| `performance`     | Too slow, too big, too much memory               |
| `sync`            | The sync protocol and the blob ledger            |
| `tech-debt`       | Work that pays down cost rather than adding      |
| `feature`         | New behaviour                                    |
| `ready-for-agent` | Fully specified, ready for an AFK agent          |
| `In Progress`     | Someone is working on it now                     |
| `In Review`       | Work is up and waiting on review                 |
| `Deferred`        | Real, but not being actioned now                 |

Kind and state are separate: a ticket usually carries one of the kind labels
(`bug`, `feature`, …) plus a state label when it has one.

## Keeping this file true

It drifted once already — it shipped as the untouched upstream template, listing
five labels of which four had never existed, and an agent following it would
have created them by accident. Check it against the tracker rather than trusting
it:

```bash
glab label list
```
