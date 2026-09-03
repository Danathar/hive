# VHS tape for `hivectl tui` — the reproducible terminal demo (tui T27, #5423).
#
# WHAT THIS RECORDS
#   The v1 TUI operator loop end to end: launch, the four populated panes and
#   the live header, focus and row navigation, the help overlay, the four write
#   actions in their SAFE states, and a clean quit. Every key pressed below and
#   every result shown is the shipped behaviour documented in
#   src/docs/hivectl.md ("tui — live terminal dashboard").
#
# PREREQUISITES
#   - VHS >= 0.10 (`brew install vhs`, or see charmbracelet/vhs). VHS needs
#     ttyd and ffmpeg; the Homebrew formula pulls both in.
#   - A Go toolchain, to build `hivectl` and run the fixture.
#   - Nothing else. No Docker, no network, no cluster, no real hive.
#
# RENDER IT (from the repository root)
#
#     cd src
#     go build -o bin/hivectl ./cmd/hivectl
#     go run ./pkg/tui/testdata/demohive &        # fixture; ctrl+c / kill to stop
#     vhs docs/design/tui.tape
#     kill %1                                     # stop the fixture
#
#   Output lands at src/docs/design/_out/tui.gif, which .gitignore excludes.
#   The TAPE is the source of truth in this repository; the GIF is a build
#   product and is NOT committed (see #5423: no generated binary by default).
#
# THE FIXTURE, AND WHY THERE IS ONE
#   `hivectl tui` is a client of the dashboard API. Recording it against a real
#   hive would burn that hive's identity, agent names, repositories and token
#   spend into a GIF in a public repository — a terminal recording is a
#   screenshot of whatever was on screen. So this tape records against
#   src/pkg/tui/testdata/demohive: a single-file, stdlib-only, loopback-only
#   HTTP server that serves fixed literals for the ~12 routes the TUI reads.
#   It refuses to bind anything but 127.0.0.1, holds no state on disk, makes no
#   outbound connection, and freezes its clock so relative timestamps render
#   identically on every run. Every name, number and dollar figure it serves is
#   invented. Read it before trusting it — it is one file.
#
#   It listens on 127.0.0.1:3001, which is already the TUI's default
#   HIVE_DASHBOARD_URL, so nothing below has to point the TUI anywhere.
#
# WHAT IS DELIBERATELY NOT RECORDED  (see also the inline notes at each point)
#   - Local tmux attach (`a`). Demonstrating it needs a live `tmux` session
#     named hive-<agent> on the recorder's machine, which is exactly the
#     "depends on a developer's existing sessions" the task rules out. The
#     tape shows the binding in the help overlay and stops there.
#   - SSE degradation and recovery. The fixture exposes no control for
#     dropping its stream on cue, and killing it mid-tape would take the poll
#     fallback down with it — which is not the state being demonstrated. A
#     deterministic drop needs a fixture control this tape does not have, so it
#     is omitted rather than faked.
#   - Completing any write. Pause is the one action carried through, against a
#     fixture agent. Model apply and ACMM apply are both driven to their
#     confirmation state and then CANCELLED, because both mutate a hive and
#     neither is safe to demonstrate as a completed act.

Output docs/design/_out/tui.gif

# 100x30 — comfortably above the TUI's documented 60x20 minimum, so the frame
# is the real 2x2 grid rather than the "terminal too small" fallback, and the
# four panes have room to show their content at a legible font size.
Set Shell "bash"
Set FontSize 16
Set Width 1400
Set Height 700
Set Padding 20
Set Theme "Catppuccin Mocha"
Set TypingSpeed 60ms
Set WindowBar Colorful
Set WindowBarSize 40

# A bare, deterministic prompt. The default shell prompt would put the
# recorder's own username, hostname and working directory into the frame.
Hide
Type `export PS1='$ '` Enter
Type "clear" Enter
Show

# ---------------------------------------------------------------------------
# 1. Launch.
#    No flags: `hivectl tui` is a bare subcommand that reads HIVE_DASHBOARD_URL
#    (default http://localhost:3001 — what the fixture is serving) and
#    HIVE_DASHBOARD_TOKEN. The fixture requires no token, so none is set here;
#    a real hive needs `export HIVE_DASHBOARD_TOKEN=...` first, and no token
#    value appears anywhere in this tape.
# ---------------------------------------------------------------------------
Type "bin/hivectl tui" Sleep 500ms Enter

# ---------------------------------------------------------------------------
# 2. All four panes and the live header.
#    Both loops fetch immediately on startup, so AGENTS / GOVERNOR / TOKENS /
#    EVENTS fill without waiting out an interval. The header reads
#    `hive: … governor: … ws: …`; `ws:` flips to `connected` only once an event
#    has actually been received on the stream, which the fixture sends at once.
# ---------------------------------------------------------------------------
Sleep 4s

# ---------------------------------------------------------------------------
# 3. Focus and navigation.
#    `tab` cycles focus forward, `shift+tab` backward; `j`/`k` move the row
#    selection in the Agents and Events panes.
# ---------------------------------------------------------------------------
Down@400ms 2
Sleep 1s
Type@400ms "k"
Sleep 1s

Tab
Sleep 1200ms
Tab
Sleep 1200ms
Tab
Sleep 1200ms
Down@400ms 3
Sleep 1500ms

Shift+Tab
Sleep 1s
Shift+Tab
Sleep 1s
Shift+Tab
Sleep 1500ms

# ---------------------------------------------------------------------------
# 4. Help overlay, and dismissal.
#    `?` toggles it; it lists every binding and dismisses on ANY key. This is
#    also where `a` (local tmux attach) is visible as a binding — see the
#    omission note in the header for why it is not exercised.
# ---------------------------------------------------------------------------
Type@400ms "?"
Sleep 3s
Escape
Sleep 1500ms

# ---------------------------------------------------------------------------
# 5a. Pause / resume confirmation.
#     `p` opens a y/n confirm rather than acting immediately. This is the one
#     write the tape carries through, and only because the target is a fixture
#     agent whose paused bit lives in the fixture's memory and is discarded
#     when it exits.
# ---------------------------------------------------------------------------
Type@400ms "p"
Sleep 2s
Type@400ms "y"
Sleep 3s

# Resume it again, so the recording leaves the fixture as it found it.
Type@400ms "p"
Sleep 2s
Type@400ms "y"
Sleep 3s

# And show the cancel path: `n` (or `esc`) backs out with no request sent.
Type@400ms "p"
Sleep 2s
Type@400ms "n"
Sleep 1500ms

# ---------------------------------------------------------------------------
# 5b. Model picker, and its qualifications.
#     `m` opens the picker for the selected agent and fetches its backend's
#     catalogue. The fixture returns a list flagged BOTH fallback (discovery
#     found nothing; these ids are guesses) and partial (some endpoints did not
#     answer, so an absence proves nothing) so the overlay's notes are visible.
#     `enter` here would APPLY and restart the agent's session — so this tape
#     stops at the qualified list and leaves with `esc`.
# ---------------------------------------------------------------------------
Type@400ms "m"
Sleep 3s
Down@400ms 1
Sleep 2s
Escape
Sleep 1500ms

# ---------------------------------------------------------------------------
# 5c. Kick, and its footer feedback.
#     `K` (capital — the shifted twin of `k` navigate, deliberately, so a
#     missed shift never fires it) queues an immediate run and reports in the
#     footer: `kick queued for <agent>`. Against the fixture this queues
#     nothing anywhere; the fixture answers 202 and discards it.
# ---------------------------------------------------------------------------
Type@400ms "K"
Sleep 3s

# ---------------------------------------------------------------------------
# 5d. ACMM overlay, reaching typed confirmation and CANCELLING.
#     `A` opens the level overlay from anywhere — the level is a property of
#     the whole hive, not the focused pane's selection. Selecting a level and
#     pressing `enter` does not apply it: it opens a typed confirmation that
#     requires the exact phrase `APPLY L<n>`, and only an exact match arms the
#     apply on the NEXT enter.
#
#     The fixture reports L2 as current, so the tape moves to a DIFFERENT level
#     to reach confirmation at all — selecting the level already in force
#     short-circuits to "nothing to apply".
#
#     The phrase is typed in full to show the overlay accepting it, and then
#     the whole thing is abandoned with `esc`. NO APPLY IS COMPLETED. `esc`
#     during confirmation backs out to the list rather than closing outright,
#     so a second `esc` closes the overlay.
# ---------------------------------------------------------------------------
Type@400ms "A"
Sleep 3s
Down@500ms 1
Sleep 1500ms
Enter
Sleep 2s
Type "APPLY L3"
Sleep 2500ms
Escape
Sleep 1500ms
Escape
Sleep 1500ms

# ---------------------------------------------------------------------------
# 6. Local tmux attach — OMITTED.
#    `a` attaches to the selected agent's tmux session (hive-<agent>) and is
#    local-only. It cannot be shown deterministically: a successful attach
#    needs a live session on the recording machine, and pressing `a` without
#    one would record the failure message rather than the feature. The binding
#    is documented in the help overlay shown at step 4 and in
#    src/docs/hivectl.md; it is not exercised here.
#
# 7. SSE degradation and recovery — OMITTED.
#    The header's `ws:` field would read `not connected` while the stream is
#    down, and the reconcile cadence would drop from 60s back to 5s. Showing
#    the transition needs the fixture to drop and restore its stream on cue,
#    which it has no control for; stopping the fixture outright would take the
#    poll fallback down with it and record an unreachable hive instead of a
#    degraded one. Omitted rather than simulated.
# ---------------------------------------------------------------------------

# ---------------------------------------------------------------------------
# 8. Clean quit.
#    `q` quits from the top level only — every overlay above was dismissed
#    first, which is the modal rule: while an overlay is open `q` is swallowed
#    like every other key.
# ---------------------------------------------------------------------------
Type@400ms "q"
Sleep 2500ms
