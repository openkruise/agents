---
trigger: always_on
alwaysApply: true
---

When running read-only git commands such as git log / git show, always pipe through head or tail. For example:

git log --oneline -15 2>&1 | head -20
git show --name-only HEAD 2>&1 | head -30
git show 62ea0c25 --stat 2>&1 | tail -100