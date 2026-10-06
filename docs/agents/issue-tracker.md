# Issue tracker: Linear

Issues and specs for this repo live in Linear. Use the Linear MCP tools (`mcp__linear__*`) for all operations.

- **Team**: Personal (key `P`)
- **Current project**: Sprut v0.1 (https://linear.app/zemliakov/project/sprut-v01-fbf046dfd678)

The Personal team also holds non-Sprut work, so always scope reads to the current project and always set it on create.

## One project = one spec

Each Linear project holds exactly one spec, and that spec is the **project description** (not an issue). Every ticket built from it is an issue in the same project, with no parent issue. Later work gets a new project with its own spec; update "Current project" above when that happens.

## Conventions

- **Create an issue**: `save_issue` with `team: "Personal"`, `project: <current project>`, `title`, `description` (Markdown, literal newlines).
- **Read an issue**: `get_issue` with the identifier (e.g. `P-123`) and `includeRelations: true`, plus `list_comments` for the conversation.
- **List issues**: `list_issues` with `project: <current project>` and `label` / `state` filters.
- **Make an issue a sub-issue of a parent**: `save_issue` with `parentId: <parent>`.
- **Comment on an issue**: `save_comment` on the issue.
- **Apply / remove labels**: `save_issue` with `addLabels` / `removeLabels` (never `labels`, which replaces the whole set). If a label doesn't exist yet, create it with `create_issue_label` scoped to team Personal.
- **Close**: `save_issue` with `state: "Done"` (or `"Canceled"` for wontfix), with a closing comment.

Statuses in Personal: Backlog, Todo, In Progress, Waiting, Done, Canceled, Duplicate. Triage state is tracked with labels (see `triage-labels.md`), not statuses.

## When a skill says "publish to the issue tracker"

- **A spec** (e.g. `/to-spec`): write it into the current project's description with `save_project` (`id: <project>`, `description`). Read the description first with `get_project`; if it holds anything beyond an empty template, ask before replacing it. Labels don't apply to a project.
- **Anything else** (tickets, bugs, requests): create a Linear issue in team Personal, in the current project. Tickets cut from the spec get no `parentId`; the project is their parent.

## When a skill says "fetch the spec"

`get_project` with the project name; the spec is its `description`.

## When a skill says "fetch the relevant ticket"

A ticket's context has three parts. Fetch all of them:

1. **The ticket**: read the issue (see Conventions).
2. **The spec**: fetch the spec (above) of the issue's `project`.
3. **Out-of-scope work**: the issues in the ticket's `blocks` relations. These are later tickets built on this one, so what they cover is deferred work, out of scope here. Their titles usually suffice; `get_issue` one whose title leaves its scope unclear.

When a skill dispatches a subagent that needs ticket context (e.g. `/code-review`'s Spec sub-agent), the identifier is the whole hand-off: pass it with a pointer to this section, and the subagent fetches all three parts itself.

## Wayfinding operations

Used by `/wayfinder`. The **map** is a parent issue with **child** issues as tickets.

- **Map**: a single issue labelled `wayfinder:map`, holding the Notes / Decisions-so-far / Fog body.
- **Child ticket**: `save_issue` with `parentId: <map>`, labelled `wayfinder:<type>` (`research`/`prototype`/`grilling`/`task`). Once claimed, the ticket is assigned to the driving dev.
- **Blocking**: Linear's native relations, `save_issue` with `blockedBy: [<P-n>, ...]`. A ticket is unblocked when every blocker is Done or Canceled.
- **Frontier query**: `list_issues` with `parentId: <map>` in open states; drop any with an open blocker or an assignee; first in map order wins.
- **Claim**: `save_issue` with `assignee: "me"`, `state: "In Progress"`, as the session's first write.
- **Resolve**: comment the answer with `save_comment`, set `state: "Done"`, then append a context pointer (gist + link) to the map's Decisions-so-far.
