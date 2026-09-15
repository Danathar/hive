## Formal verification capability — operator enabled

`quality.formal` is enabled and this hive is at ACMM L5 or above. Formal work
is optional per repository and is limited to protocol and liveness properties;
do not try to prove general functional correctness or model every subsystem.

### Admission: justify model-worthiness first

Author a model only when the target has protocol-shaped behavior with one or
more concrete signals such as a state-machine lifecycle, concurrent actors,
retry/escalation/ledger state, ordering or lock invariants, lease/claim
ownership, or a prior protocol incident. In the model PR, name the signals and
explain why ordinary unit tests cannot reliably cover the relevant
interleavings. If no subsystem qualifies, continue ordinary quality work and do
not add placeholder formal artifacts.

### Artifact and verification contract

- Put each model at `formal/<subsystem>/{model.pml,run.sh,README.md}`. A repo
  whose source lives below a component root may keep `formal/` at that root.
- `README.md` must map every modeled state and transition to its concrete code
  construct, state all abstractions and fairness assumptions, identify every
  invariant/property, and give local counterexample replay commands.
- `run.sh` must run exhaustive Spin verification and exit nonzero on property
  drift. Pin both expected-pass properties and intentional expected-fail
  witnesses: a regression and a silent fix that leaves stale expectations must
  both turn the script red.
- Add a path-gated, reporting-only CI job for `formal/**`, the modeled source,
  and the workflow itself. It must never become a required merge check. Preserve
  enough logs/trails to replay a failure; do not publish a raw trail as the
  issue explanation.

### One actionable issue per violated property

For each unexpected violation, minimize/replay the counterexample and translate
it into a plain-language actor-by-actor interleaving. Use the stable title
`[formal:<subsystem>:<property-id>] <property summary>`. A narrow exact-title
lookup for that title is allowed solely for deduplication. Create the issue if
none is open; otherwise update the existing issue with the new model revision,
failing run, and narrative. Never create one issue per run, and never combine
distinct properties into one issue. Include:

- the violated property and user impact;
- the minimized interleaving, naming each actor and state transition;
- the model revision plus the failing CI run or reproducible local command;
- the corresponding production code paths; and
- whether a candidate fix was modeled and what the patched model proved.

Use the repository's issue-write helper when one is provided. Do not grant a
pull-request workflow broad issue-write permission merely to automate filing;
the quality agent owns narrative construction and exact-title deduplication.

### Maintenance rule

When a PR touches a modeled subsystem and formal verification turns red, fix
the code or update the model in that same PR with the changed invariant story.
Do not rerun past the evidence, weaken an invariant, delete an expected witness,
or narrow the path gate just to make CI green. Existing red PR repair remains
FIX-BEFORE-NEW and takes precedence over authoring another model.
