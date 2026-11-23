# Decision graph

Goals and decisions, what they're based on, and the tasks that carry them out, kept as context for people and agents. In-house; built on what ktx already stores instead of a separate graph.

## Model

- **Goals and decisions are notes.** A note may have a kind (`goal` or `decision`), a status, a strength and "based on" links. A note without a kind is a plain note.
  - An *option* is a decision with status `considering`; a *preference* is a goal, usually `tentative`.
- **Context is the notes and transcripts that already exist.** How reliable a note is comes from its labels (`slop` = unreviewed).
- **Tasks are the existing tasks** (VTODOs). A task can say which goal or decision it's for.
- **One kind of link: "based on"**, kept on the note that depends on the other. It points at a note or a transcript, with an optional verbatim quote. Links between notes can't form a loop.

```yaml
# notes/<date>-<time>-<slug>-<id>.md, front matter (new fields; all optional)
kind: decision          # goal | decision
status: made            # goal: active, achieved, paused, dropped
                        # decision: considering, made, rejected, reversed
strength: firm          # hard | firm | tentative (goals and decisions)
private: true           # only its author sees it in ktx (it's still in git)
based_on:
  - ref: note:2702d58ac84d
  - ref: transcript:f144dccdc869
    quote: advisors get you into the right rooms
```

```
X-KTX-FOR:note:2702d58ac84d   # in a VTODO: the goal or decision the task is for
```

## Views

- **Notes:** the scratchpad and the edit form take a kind, status, strength, "based on" links (searched by title) and private. The list filters by kind. A note's page shows what it's based on (with quotes), what's based on it, and the tasks for it. Notes and transcripts have "Add a goal or decision based on this".
- **Graph** (`/graph`): goals and decisions as an outline. At the top are the ones not based on another (visible) goal or decision; under each, those based on it, then its context (notes and transcripts, with quotes) and its tasks. A node with several parents is shown in full under the first and as "also under" elsewhere. Finished ones (achieved, dropped, rejected, reversed) are hidden unless asked for.
- **Tasks:** a task's page picks which goal or decision it's for; the list shows it.
- **MCP:** `get_graph` (the outline as compact text), plus kind, status, strength and links on `search_notes`/`get_note`; `search_notes` filters by kind. Read-only for the graph: `create_note` makes plain notes labelled slop.

## Private notes

A private note is shown only to its author: in the notes list, its page, search, the graph, transcript pages, the task picker and MCP. Others see a link to it as "a private note". It's still a file in company-brain, so it's hidden, not secret.

## Later

1. Review flags: when a goal, decision or note changes, mark what's based on it for review.
2. A drawn graph.
3. Agents propose changes; you see a visual diff and approve or deny.
4. Links to code.
