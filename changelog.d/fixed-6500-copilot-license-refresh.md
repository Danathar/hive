- Fixed Copilot agents incorrectly reporting that they had no license when a
  stale or unrelated account remained in the shared CLI config. Explicit
  `COPILOT_GITHUB_TOKEN` credentials and fresh dashboard logins now replace
  stale CLI identities, while multi-account configs promote only the selected
  account instead of whichever token Go map iteration returned first
  ([#6500](https://github.com/hivecommons/hive/issues/6500)).
