---
name: create-upstream-issue
description: Create issues on the upstream OpenKruise Agents repository (https://github.com/openkruise/agents) using GitHub CLI. Reads the local codebase, performs preliminary analysis, then creates a well-structured issue with background, issue type, relevant code references, and notes. Use when the user wants to report a bug, request a feature, or raise an issue to the upstream openkruise/agents project. Triggered by phrases like "提 issue", "create upstream issue", "上报 issue", "提交 issue 到上游", or "report to upstream".
---

# Create Upstream Issue

Create well-structured issues on the upstream [openkruise/agents](https://github.com/openkruise/agents) repository via
GitHub CLI. This skill reads the local codebase, analyzes the problem or feature in context, then creates the issue
directly — no manual copy-pasting.

## When to Use

Trigger when the user wants to:

- Report a bug found in the project to upstream
- Request a feature or enhancement
- Raise a discussion topic for the upstream maintainers
- 提 issue / 提交 issue / 上报 issue / 创建 issue / report to upstream

## Prerequisites

Before proceeding, verify:

1. **GitHub CLI is installed and authenticated:**
   ```bash
   gh auth status
   ```
   If not authenticated, instruct the user to run `gh auth login`.

2. **The upstream repository is accessible:**
   ```bash
   gh repo view openkruise/agents --json name
   ```
   This should succeed. If not, the skill still works — `gh issue create` targets `openkruise/agents` directly.

## Workflow

### Step 1: Understand the User's Intent

Ask clarifying questions if the request is vague:

- What is the exact problem or feature?
- When / under what conditions does it occur?
- Is this a bug, feature request, or discussion?

Classify the issue type:
- `bug` — unexpected behavior, crash, error, performance issue
- `feature` — new capability, enhancement request
- `discussion` — architectural question, design debate, proposal pre-discussion

### Step 2: Gather Context from Codebase

Read the local codebase to gather relevant context. Use these tools in parallel where possible:

1. **Search for related code** using `search_codebase` or `grep_code` based on keywords from the user's description.
2. **Read relevant files** — focus on the core logic, not boilerplate.
3. **Check related CRD types** in `api/v1alpha1/` if the issue involves API changes.
4. **Check proposals** in `docs/proposals/` for existing discussions on similar topics.

Keep code references **concise** — include only the minimum context needed to understand the issue.

### Step 3: Draft the Issue

Use this template structure:

```markdown
### Background
[1-3 sentences describing the context and what the user wants to achieve or what problem they encountered]

### Issue Type
[bug / feature / discussion]

### Relevant Code (if applicable)
[Concise code references — file paths and minimal snippets. Only include what is necessary to understand the issue.]

### Notes
[Optional: edge cases, constraints, related proposals, or anything that helps understand the scope without prescribing a solution]
```

**Critical rules for issue content:**

- **DO NOT** propose solutions or implementation approaches
- **DO NOT** make subjective judgments about what should be done
- Keep the issue **open-ended** — describe the problem or need, not the fix
- Use factual, neutral language
- Code snippets should be minimal — a few lines at most. Prefer file path references over large code blocks

### Step 4: Review with the User

Show the drafted issue (title + body) to the user and ask for confirmation. Allow the user to:

- Edit the title or body
- Add labels (e.g., `bug`, `enhancement`, `discussion`)
- Cancel creation

Do not proceed to creation until the user confirms.

### Step 5: Create the Issue

Once confirmed, create the issue:

```bash
gh issue create \
  --repo openkruise/agents \
  --title "<title>" \
  --body "<body>"
```

If the user wants labels:
```bash
gh issue create \
  --repo openkruise/agents \
  --title "<title>" \
  --body "<body>" \
  --label "bug"
```

**IMPORTANT:** The `--body` content may contain special characters (backticks, quotes, newlines). Use a temporary file to avoid escaping issues:

```bash
cat > /tmp/issue-body.md << 'ISSUE_EOF'
<body content here>
ISSUE_EOF

gh issue create \
  --repo openkruise/agents \
  --title "<title>" \
  --body-file /tmp/issue-body.md
```

### Step 6: Report the Result

After creation, report:
- The issue URL
- The issue number
- Suggest any follow-up actions (e.g., linking a PR later)

## Issue Title Guidelines

- Bug: `bug: <concise description>`
- Feature: `feat: <concise description>`
- Discussion: `discussion: <concise description>`

Keep titles under 80 characters when possible. Use English for the title.

## Error Handling

| Scenario | Action |
|----------|--------|
| `gh` not installed | Tell user to install: `brew install gh` |
| `gh` not authenticated | Tell user to run: `gh auth login` |
| `gh issue create` fails | Show the error output. Check for special characters in the body — use `--body-file` with a temp file |
| Cannot access repo | Verify the user has access to `openkruise/agents`. The repo is public, so this should not normally fail |
| User cancels | Stop — do not create the issue |

## Example

**User:** "我在 SandboxSet controller 里发现一个问题，当 sandbox 数量很多的时候 reconcile 会越来越慢，帮我提个 issue"

**Agent workflow:**

1. Classify as `bug` (performance issue)
2. Search codebase for reconcile logic with `search_codebase` or `grep_code`
3. Read relevant controller file(s) in `pkg/controller/`
4. Draft issue:

   ```
   Title: bug: SandboxSet controller reconcile slows down at scale

   ### Background
   When a SandboxSet manages a large number of Sandboxes, each reconcile loop
   takes progressively longer, causing delays in processing new changes.

   ### Issue Type
   bug

   ### Relevant Code
   pkg/controller/sandboxset_controller.go — the reconcile loop processes all
   managed Sandboxes sequentially without pagination or incremental processing.

   ### Notes
   - Observed with 100+ Sandboxes under a single SandboxSet
   - Each reconcile appears to re-process all Sandboxes from scratch
   ```

5. Show to user for confirmation
6. Create issue on `openkruise/agents`
