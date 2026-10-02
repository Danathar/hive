# Dashboard feedback

The spoke dashboard has a **Feedback** button in the avatar menu, a floating button, a top-bar 🐛 button, and a **Report an Issue** item in the left sidebar. Use it to report a bug or request a feature; each submission becomes a GitHub issue in `hivecommons/hive`, or `hivecommons/docs` when you choose **Documentation**.

Submissions include the title, description, feedback type, target repository, and optional pasted or uploaded screenshots. Diagnostics are optional and previewed before submit. They include only non-sensitive dashboard/browser context such as Hive version, ACMM level, Hive ID, hub-linked status, agent backend/model/state, browser details, recent console errors, and failed `/api/*` calls. Tokens, bearer values, emails, and secret-like key/value text are redacted before sending.

Hub-linked spokes relay feedback to the hub so the hub can create the issue with its GitHub credentials. Standalone spokes use the signed-in dashboard user's GitHub auth when available; otherwise the dashboard opens a prefilled GitHub issue URL and asks you to paste screenshots manually.

The 🐛 button also shows a notification pill for feedback filed from this hive. The spoke stores created issue references in `/data/feedback-submissions.json`, polls GitHub activity through the hub relay or the signed-in user's token, and counts issues whose `updated_at` is newer than the last time the **My reports** tab was viewed. Fallback-URL submissions are not tracked until GitHub issue creation succeeds through the dashboard.
