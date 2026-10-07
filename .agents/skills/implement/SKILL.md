---
name: implement
description: "Implement a piece of work based on a spec or set of tickets."
disable-model-invocation: true
---

Implement the work described by the user in the spec or tickets.

1. Call the Skill tool with "tdd" where possible, at pre-agreed seams. Run typechecking regularly, single test files regularly, and the full test suite once at the end.

2. Once every acceptance criterion has a passing test, call the Skill tool with "simplify", then commit to the current branch.

3. Call the Skill tool with "code-review" from the commit before this work's first commit. It diffs commits, so uncommitted and untracked files escape it. Review fixes go in a follow-up commit.

4. Post the decisions the ticket left open as a comment on the issue, so the reviewer and later tickets see them.

5. Finish by moving the issue to Done with a closing comment.
