---
name: create-upstream-pr
description: Create pull requests to the upstream OpenKruise Agents repository (https://github.com/openkruise/agents) using GitHub CLI. Analyzes the current branch against upstream master for conflicts, reviews branch changes, then creates a well-structured PR body focusing on core changes with review guidance. Incidental changes unrelated to the main purpose are listed in an appendix at the end. Use when the user wants to create a PR, submit code to upstream, or merge changes to openkruise/agents. Triggered by phrases like "提 PR", "create upstream PR", "提交 PR", "create pull request", "向上游提 PR".
---

# Create Upstream PR

Create structured pull requests targeting the upstream [openkruise/agents](https://github.com/openkruise/agents) repository via GitHub CLI. This skill checks for merge conflicts with upstream master, analyzes branch changes categorizing core vs incidental modifications, and generates a PR body with review guidance — no manual copy-pasting.

## When to Use

Trigger when the user wants to:

- Submit a branch's changes as a PR to upstream
- Create a pull request from the current feature branch
- 提 PR / 提交 PR / 创建 PR / 向上游提 PR / create upstream PR

## Prerequisites

Before proceeding, verify:

1. **GitHub CLI is installed and authenticated:**
   ```bash
   gh auth status
   ```
   If not authenticated, instruct the user to run `gh auth login`.

2. **Upstream remote is configured:**
   ```bash
   git remote get-url upstream
   ```
   If not configured, add it:
   ```bash
   git remote add upstream https://github.com/openkruise/agents.git
   ```

3. **Current branch is not master/main:**
   ```bash
   git branch --show-current
   ```
   If on master, warn the user to switch to a feature branch first.

## Workflow

### Step 1: Fetch and Check Conflicts

Fetch the latest upstream master and check for merge conflicts:

```bash
git fetch upstream master
```

Check for conflicts by attempting a merge without committing:

```bash
git merge --no-commit --no-ff upstream/master
```

If the merge has conflicts, the command will exit non-zero. Abort immediately:

```bash
git merge --abort
```

**If conflicts exist:** Stop here. Tell the user:
- The current branch has merge conflicts with upstream master
- They need to resolve conflicts first (e.g., `git rebase upstream/master`)
- Do NOT proceed to PR creation

**If no conflicts:** Abort the dry-run merge and continue:
```bash
git merge --abort
```

### Step 2: Analyze Branch Changes

Collect information about the branch's changes:

1. **Get the fork point** (where the branch diverged from upstream/master):
   ```bash
   git merge-base HEAD upstream/master
   ```

2. **Get changed files summary:**
   ```bash
   git diff --stat upstream/master...HEAD
   ```

3. **Get commit log** (branch-only commits):
   ```bash
   git log upstream/master..HEAD --oneline
   ```

4. **Get the detailed diff:**
   ```bash
   git diff upstream/master...HEAD
   ```

### Step 3: Categorize Changes

Analyze the diff and commit messages to categorize changes:

- **Core changes:** Modifications directly related to the main purpose of the branch. These are the primary reason for the PR.
- **Incidental changes (appendix):** Side modifications like import reordering, typo fixes, minor refactors in unrelated files, go.mod/go.sum updates not tied to the main change, formatting fixes, etc.

Principles for categorization:

- Read commit messages to understand the intent of each commit
- Group related file changes by their functional purpose
- If a change would be distracting in the main review, put it in the appendix
- When in doubt, ask the user whether a change is core or incidental

### Step 4: Draft PR Body

Read and follow the project's PR template at [`.github/PULL_REQUEST_TEMPLATE.md`](../../.github/PULL_REQUEST_TEMPLATE.md). The template has four sections: Ⅰ Describe what this PR does, Ⅱ Does this pull request fix one issue, Ⅲ Describe how to verify it, and Ⅳ Special notes for reviews.

If there are incidental changes, append an appendix section at the end:

```markdown
---

### Appendix: Incidental Changes

The following changes are side modifications not directly related to the main purpose of this PR:

- `<brief description of change 1>` (`<file paths>`)
- `<brief description of change 2>` (`<file paths>`)
```

**Rules for PR body content:**

- **DO** focus on what changed and why — not implementation minutiae
- **DO** highlight areas that need careful review
- **DO** keep the appendix factual and brief — no justification needed for minor cleanups
- **DO NOT** include generated files in the main description (list in appendix or omit)
- **DO NOT** write a novel — reviewers should understand the change in under a minute

### Step 5: Determine PR Title

Auto-generate the PR title based on the branch changes:

- Use conventional commit format when possible: `feat(<scope>): <description>` or `fix(<scope>): <description>`
- If the branch name follows a pattern like `feature/xxx` or `fix/xxx`, use it as a hint
- If the branch has a single clear purpose, derive the title from the main commit message
- Keep the title under 80 characters

### Step 6: Review with the User

Show the drafted PR (title + body) to the user for confirmation. Allow the user to:

- Edit the title or body
- Adjust the categorization (move items between core and appendix)
- Set base branch if not `master`
- Cancel creation

Do not proceed to creation until the user confirms.

### Step 7: Create the PR

Once confirmed, create the PR using the `--body-file` pattern for safety:

```bash
cat > /tmp/pr-body.md << 'PR_EOF'
<body content here>
PR_EOF

gh pr create \
  --repo openkruise/agents \
  --base master \
  --head "<current-branch>" \
  --title "<title>" \
  --body-file /tmp/pr-body.md
```

If the user's fork uses a different remote name, adjust `--head` accordingly:

- If pushing from a fork: `--head <username>:<branch>`
- If pushing from upstream directly: `--head <branch>`

### Step 8: Report the Result

After creation, report:

- The PR URL
- The PR number
- Suggest any follow-up actions (e.g., requesting specific reviewers, linking issues)

## Conflict Checking Details

The conflict check in Step 1 uses `git merge --no-commit --no-ff upstream/master`. Common scenarios:

| Scenario | `git merge` result | Action |
|----------|-------------------|--------|
| Clean merge | Exits 0, no conflict markers | Continue to Step 2 |
| File-level conflict | Exits 1, lists conflicted files | Abort, tell user to rebase |
| Merge already in progress | Error about MERGE_HEAD | Run `git merge --abort` first |

Always run `git merge --abort` after the check to restore the working tree.

## Analysis Guidelines

### Identifying the Main Purpose

When analyzing changes, ask these questions:

1. What is the branch name? (e.g., `fix/api-key-delete`, `feat/sandbox-metrics`)
2. What do the commit messages say?
3. Which files are most heavily modified?
4. Are there proposal documents in `docs/proposals/` that match?

The answer forms the **core changes**.

### What Goes in the Appendix

Examples of incidental changes:

| Change Type | Example |
|-------------|---------|
| Import reordering | `goimports` reordering imports in unchanged files |
| Typo fixes in comments | Fixing a typo in a file otherwise untouched |
| Go module changes | `go.mod` / `go.sum` updates from `go mod tidy` |
| Formatting | Whitespace, line wrapping in unrelated areas |
| Test-only helper updates | Adding a test helper used by the main change but in a shared file |

### What Should Be Core (Not Appendix)

- New types, functions, or logic
- Modified business logic in existing functions
- Test cases for the new/modified functionality
- Configuration changes that are part of the feature

## Error Handling

| Scenario | Action |
|----------|--------|
| `gh` not installed | Tell user to install: `brew install gh` |
| `gh` not authenticated | Tell user to run: `gh auth login` |
| No `upstream` remote | Add it: `git remote add upstream https://github.com/openkruise/agents.git` |
| On master branch | Warn user to switch to a feature branch |
| Merge conflicts detected | Stop — tell user to rebase onto upstream/master first |
| `gh pr create` fails | Show the error output. Check `--head` format and authentication |
| User cancels | Stop — do not create the PR |

## Examples

### Example 1: Single-purpose branch

**User:** "帮我把当前分支的改动提个 PR 到上游"

**Branch name:** `fix/api-key-delete-idempotent`

**Agent workflow:**

1. Check conflicts: clean merge → proceed
2. Get fork point and diff:
   ```
   git merge-base HEAD upstream/master  →  abc1234
   git diff --stat upstream/master...HEAD →
     pkg/servers/e2b/keys/secret.go       | 12 ++++++------
     pkg/servers/e2b/keys/secret_test.go  | 8 ++++----
   git log upstream/master..HEAD --oneline →
     abc1234 fix: make DeleteKey idempotent (return 204 on missing resource)
   ```
3. Categorize: All changes are core (single purpose)
4. Draft PR:

   ```
   Title: fix(keys): make DeleteKey idempotent — return 204 on missing resource

   ### Ⅰ. Describe what this PR does

   Changes DeleteKey to be idempotent: deleting a non-existent API key now
   returns 204 instead of an error. This aligns with RESTful conventions and
   prevents client-side retry issues.

   ### Ⅱ. Does this pull request fix one issue?

   NONE

   ### Ⅲ. Describe how to verify it

   1. Run unit tests: `go test ./pkg/servers/e2b/keys/...`
   2. Verify that deleting a non-existent key returns 204 (not 404)
   3. Verify that deleting an existing key still works correctly

   ### Ⅳ. Special notes for reviews

   - The 204 return uses `k8s.io/apimachinery/pkg/api/errors.IsNotFound()` to
     detect the not-found case
   - The retry helper in `retryUpdateSecret` already handles the delete path
     correctly — no changes needed there
   ```

5. User confirms → create PR

### Example 2: Mixed changes with incidental modifications

**Branch name:** `feat/sandbox-prometheus-metrics`

**Agent workflow:**

1. Check conflicts: clean merge → proceed
2. Analyze diff:
   ```
   git diff --stat upstream/master...HEAD →
     pkg/sandbox-manager/infra/metrics.go        | 85 +++++++++++++++++++
     pkg/controller/sandbox_controller.go         | 12 +--
     pkg/utils/constants.go                       |  2 +-
   ```
3. Analyze commit log:
   ```
   feat(sandbox-manager): add Prometheus metrics for sandbox ops
   chore: fix import ordering in sandbox controller
   chore: update debug log level constant name
   ```
4. Categorize:
   - **Core:** `pkg/sandbox-manager/infra/metrics.go` — new metrics
   - **Incidental (appendix):** import reordering in `sandbox_controller.go`, constant rename in `constants.go`
5. Draft PR body with appendix. User confirms → create PR
