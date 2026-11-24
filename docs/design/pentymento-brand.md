# Pentymento — Brand Guidelines

Nov 24, 2026 · @Toluwalase Akinyemi

## 1. Brand at a glance

Pentymento is a calm, precise, honest tool for undoing database mistakes, and every part of the brand — from the logo to the CLI's error messages — follows from that.

| Element | Decision |
| --- | --- |
| Product name | Pentymento (pronounced *pen-tih-MEN-toh*) |
| Binary | `pentym` (pronounced *PEN-tim*) |
| Tagline | **Undo the DROP.** |
| Subtitle | Point-in-time recovery and undo-DROP for self-hosted Postgres and MongoDB, in a single binary. |
| Metaphor line | Every earlier version, still underneath. |
| Lead pillar | Undo the mistake |
| Tone | Calm, precise, honest, helpful |
| Logo | D1: a counter-clockwise arc around three even layers |
| Brand colour | Indigo `#4F46E5` (light) / `#A5B4FC` (dark) |
| Fonts | Inter (headings and body), JetBrains Mono (code) |
| Repository | `github.com/HakeemTheEmperor/pentymento` |
| Domain | `pentymento.dev` |
| Trademark | Pentymento™, unregistered; forks use a different name |

These guidelines cover the product's identity, voice, visuals and presence. They complement the [PRD](https://claude.ai/artifact/F82YZzwaeTWprEJJ5VsjXP) and the [design document](https://claude.ai/artifact/3H5XENx5KYN8YBcveLCRJf), and use the same terms.

## 2. Name

Anything a person reads says **Pentymento**; anything a machine reads says **`pentym`**.

### 2.1 Pentymento (the product)

- Used in prose, titles, the README, docs and announcements.
- Capitalized as a normal name: Pentymento. Never PentyMento, never "Pentimento"; all caps only if a logo treatment requires it.
- No abbreviations in prose (no "PM", no "Penty").
- Possessive is fine ("Pentymento's repository format"). Not used as a verb.

### 2.2 `pentym` (the binary and commands)

- Always lowercase, in code formatting when it refers to something typed or seen in a terminal: "run `pentym restore`".
- "The pentym agent" is acceptable for the running process; "Pentymento" is the product as a whole.

### 2.3 Identifier family

| Where | Form |
| --- | --- |
| Environment variables | `PENTYM_…` |
| Database roles | `pentym_backup`, `pentym_repl`, `pentym_restore` |
| Canary schema | `pentym.canary` |
| Metrics | `pentym_…` |
| Webhook header | `X-Pentym-Signature` |
| Service user | `pentym` (Linux), `_pentym` (macOS), `NT SERVICE\pentym` (Windows) |
| Config paths | `/etc/pentym/`, `%ProgramData%\pentym\` |
| Message IDs | `PTM-…` |

### 2.4 Pronunciation

- **Pentymento:** *pen-tih-MEN-toh*, the same as the Italian word; the y is spelling only.
- **`pentym`:** *PEN-tim*.

The README states both in one line.

## 3. Tagline and story

The brand speaks at three depths: a line that hooks, a line that explains, and a story that gives it meaning.

### 3.1 The three lines

| Line | Text | Where |
| --- | --- | --- |
| Tagline | **Undo the DROP.** | README header, social preview, talks, demo video title |
| Subtitle | Point-in-time recovery and undo-DROP for self-hosted Postgres and MongoDB, in a single binary. | GitHub description, README under the tagline, package descriptions |
| Metaphor line | Every earlier version, still underneath. | "Why the name" paragraph, docs landing page |

The subtitle names only engines that ship; MySQL is added when v2 ships. Taglines never use "never lose data", "guaranteed" or "instant".

### 3.2 The story of the name

*Pentimento* is Italian, from *pentirsi*, "to repent" or "to change one's mind". In painting it is a trace of an earlier version that shows through after the artist painted over it.

**One line** (docs landing page):

> Named after *pentimento*: the earlier version of a painting that still shows through the paint.

**Short paragraph** (README "Why the name", about page):

> In painting, a *pentimento* is a trace of an earlier version showing through — the artist changed their mind and painted over it, but the original is still there underneath. Databases get painted over all the time: rows are updated, tables are dropped, mistakes are made. Pentymento keeps every earlier layer recoverable, so when you change your mind, you can bring back exactly what was there.

**Long form** (launch write-up only): the paragraph above, plus why the spelling is stylized — *pentimento* is a common word shared with art history and a well-known memoir, so the y makes the project distinct and searchable while keeping the pronunciation.

### 3.3 Rules

- The story says earlier layers are *recoverable*, never that "nothing is ever lost".
- The former working name and the naming collisions stay out of user-facing material; at most a short note in the launch write-up.
- The painting metaphor lives in the name story only, not in headings, error messages or product imagery.

## 4. Messaging pillars

Four claims carry all messaging; Pillar 1 always leads, and Pillars 2–4 are the reasons to trust it.

| Pillar | Claim | Proof points | Don't claim |
| --- | --- | --- | --- |
| **1. Undo the mistake** | Recover a dropped table or collection to just before the drop, without throwing away the rest of the database's writes | Continuous WAL and oplog capture; event detection (`pentym events`); `restore --before-event`; side-instance restore and extraction; recovery point in seconds | That every mistake is detected. v1 detects DROP, TRUNCATE and dropDatabase on both engines and bulk deletes on MongoDB; Postgres bulk DML is v1.x |
| **2. Backups you can prove** | Pentymento checks that backups restore, to the point it says they reach | `verify` L0–L5 including the recovery point reached; `check`; dead-man's switch; `status` shows the actual recovery window | "Guaranteed" backups. Backups are tested and problems are reported |
| **3. Secure by design** | A compromised database host can't read or delete backup history | Write-only agents; separate storage credentials; always-on encryption; Object Lock support; no secrets in command lines or logs; `doctor` flags over-privilege | "Unhackable", or anything beyond the published threat model |
| **4. One binary, no babysitting** | Easy to start, safe to leave running | Single binary for Linux, macOS and Windows; `init` in about two minutes; built-in scheduler; runs as a service; restores never depend on the agent; constant memory; deduplication; actionable alerts | "Zero configuration". It needs a database role, a storage location and a key |

- Short formats (social posts, a slide) use Pillar 1 plus one other.
- Benchmark numbers become evidence for Pillars 1, 2 and 4 only after they are measured; no figure appears in marketing text before then.

## 5. Audience and tone

Pentymento is written for people who may be reading it in the middle of an incident, so it stays calm, exact and useful.

### 5.1 Audience

| Audience | What they want | Priority |
| --- | --- | --- |
| Self-hosters and solo developers (VPS, Docker, bare metal) | Set up in minutes; trust it when stressed | Primary |
| Small teams and startups with a few self-hosted databases | Production confidence: multiple targets, alerts, CI, least privilege | Primary |
| Experienced database and infrastructure engineers evaluating it | Precise detail: detection, format, threat model, honest limits | Secondary, hardest to win |

### 5.2 Tone

| Trait | Means | Not |
| --- | --- | --- |
| **Calm** | Steady and matter-of-fact, especially in errors and alerts | Alarmist, dramatic, exclamation marks |
| **Precise** | Exact numbers, names, commands; engine and version named | Vague reassurance |
| **Honest** | Limits, gaps and what isn't covered, stated up front | Hype, "guaranteed", "never lose data" |
| **Helpful** | Every problem comes with the next step | Blame, dead ends |

### 5.3 By context

- **Alerts and errors:** shortest and calmest — what happened, impact, next command.
- **Docs:** second person, plain words, examples first, depth layered.
- **README and launch:** confident but evidenced; the demo and measured numbers before adjectives.
- **Issues and community:** friendly and appreciative; no promised dates or undecided features.

### 5.4 Words

- **Avoid:** blazing fast, revolutionary, magic, seamless, guaranteed, never lose data, simply, just ("just run…"), humour in error messages.
- **Use consistently:** the glossary terms in section 12.

## 6. Logo

The mark is **D1**: a counter-clockwise arc around three even layers — versions stacked, and a way back through them. It is understood at a glance; the name carries the painting metaphor, so the mark doesn't have to.

&#91;embedded content: D1 logo concept · primary, single-colour and small-size versions\]

The drawing shows the concept in this document's accent colour; the exact brand values are in section 7. Final geometry is set when the asset kit is built.

### 6.1 Construction

- Three equal layers, evenly spaced, and one counter-clockwise arc with an arrowhead, open on the right.
- **Monochrome by design:** the arc in the brand colour and the layers in the deep brand shade, or everything in one colour. No layer is highlighted.
- **Small sizes:** below 32 px, a simplified version with heavier strokes (and possibly two layers), confirmed at the asset-building stage.
- **Lockup:** the mark beside the "Pentymento" wordmark in Inter Semibold, outlined in the files.

### 6.2 Rules

- **Clear space:** at least the height of one layer on every side.
- **Minimum size:** full D1 down to 32 px; the small version below that.
- **Never a clock face** — rewind-around-a-clock is a common backup-tool image (for example Apple's Time Machine). The layers make the mark ours.
- Don't recolour outside the palette, stretch, rotate, add shadows, gradients or outlines, change the number of layers, or place the mark on busy backgrounds.
- No painting props (brushes, palettes, easels) in or around the mark.

## 7. Colour

Indigo is the brand colour; status colours are conventional and kept separate, because they carry meaning.

| Role | Light mode | Dark mode | Used for |
| --- | --- | --- | --- |
| Brand | `#4F46E5` | `#A5B4FC` | Logo arc, links, headings, info messages |
| Brand, deep | `#3730A3` | `#C7D2FE` | Logo layers, text on light tints |
| Ink | `#1C1B22` | `#ECEBF2` | Body text, single-colour logo |
| Muted | `#5B5A66` | `#A09FAB` | Secondary text |
| Surface | `#FAFAF7` | `#16151B` | Backgrounds, social preview, terminal theme |
| OK | `#1F9D57` | `#4ADE80` | Success states |
| Warning | `#C98A0B` | `#FBBF24` | Warnings |
| Critical | `#D14343` | `#F87171` | Errors, failures, gaps |

### 7.1 Rules

- **Stay clear of the engines' colours** (Postgres slate blue, MongoDB green) so Pentymento never looks like part of either project.
- **Status colours are only for status** — never decorative.
- **Colour is never the only signal:** every status also has a word or symbol.
- **Contrast:** every text colour must pass WCAG AA against its background; checked when assets are built.
- **Terminal:** the CLI uses standard ANSI colours (section 10), not these hex values.
- **Earth tones** may appear only in "Why the name" or launch illustrations, never in product UI or the CLI.

## 8. Typography

Inter for everything people read, JetBrains Mono for everything they type; both are open-licensed (SIL OFL).

| Use | Font | Weight |
| --- | --- | --- |
| Wordmark ("Pentymento" beside the mark) | Inter | Semibold, slightly tightened letter spacing; outlined in logo files |
| Headings | Inter | Semibold |
| Body | Inter | Regular |
| Code, commands, CLI screenshots, demo video | JetBrains Mono | Regular; bold for emphasis |

- **Fallbacks:** `system-ui` for sans and `ui-monospace` for mono, so the docs render instantly.
- **The CLI** uses the user's terminal font; JetBrains Mono applies only to Pentymento's own screenshots and recordings.
- Font licence files travel with any font files the project ships.
- Inter is widely used, so the type is deliberately neutral; the mark and the indigo carry the identity.

## 9. Imagery

Show the real product, flat and labelled, and decorate nothing.

### 9.1 Diagrams

- Flat and labelled, as in the PRD and design document: rounded boxes, thin connectors, no shadows, gradients or 3D.
- The title states the finding, not the topic ("Restores read only the repository; the agent is optional").
- Neutral greys for structure, indigo for the one thing the diagram is about, status colours only for status.
- Inter for labels; commands and identifiers in JetBrains Mono.
- Sources kept in `docs/diagrams/`.

### 9.2 Terminal screenshots

- Real output from real runs, never mocked or retouched.
- One setup: JetBrains Mono, a dark theme from the palette (surface `#16151B`, ink text, indigo prompt), about 100 columns wide, the same prompt every time.
- Demo hostnames, example credentials and placeholder bucket names only.
- Prefer copyable text in docs; images only where colour or layout matters.

### 9.3 Demo video and GIF

- Scripted and reproducible with a terminal recorder such as VHS or asciinema.
- Follows the two-act demo: drop a table, receive the alert, restore before the event; then the same for MongoDB.
- About 60–90 seconds for the README GIF; a longer narrated version for the launch write-up.
- Real timings; any speed-up is stated on screen.

### 9.4 Social preview and banner

- GitHub social preview, 1280 × 640: D1 mark, "Pentymento" wordmark, **Undo the DROP.**, and the subtitle on the surface colour; light and dark variants.
- README banner: the same composition at a wider, shorter crop.

### 9.5 Never used

Stock photos, 3D renders, AI-generated artwork, robot or cloud clichés, and painting props (brushes, palettes, easels).

### 9.6 Accessibility

Every image has alt text; diagrams carry a one-sentence reading beneath; the demo GIF has a text transcript of its commands.

## 10. CLI output style

The CLI is where most people meet the brand: it reads well without colour, stays quiet by default, and never gets in the way of scripts.

### 10.1 Principles

1. **Words first, colour second:** every status is a word, with a symbol and colour as extra cues.
2. **stdout for results, stderr for everything else:** tables, IDs and `--json` on stdout; progress, spinners and logs on stderr.
3. **Quiet by default:** the result and the next step; detail behind `-v`.

### 10.2 Colour

| Meaning | ANSI | Used for |
| --- | --- | --- |
| OK | green | `✓` lines, passed checks |
| Warning | yellow | `!` lines, stale agent, low window |
| Critical / failed | red | `✗` lines, errors, gaps |
| Emphasis | bold | target names, the next command |
| Secondary | dim | timestamps, IDs, hints |
| Brand | — | Indigo only in the `--version` banner, if anywhere |

- Colour only on a TTY; off when piped, redirected or in CI unless forced.
- `--color=auto|always|never` (default `auto`); `NO_COLOR` turns colour off; `--json` never contains colour codes.
- Standard ANSI colours only, so output follows the user's terminal theme.

### 10.3 Symbols

- `✓` OK · `!` warning · `✗` failed · `→` next step · `…` in progress.
- ASCII fallback (`OK`, `WARN`, `FAIL`, `->`) chosen automatically when Unicode can't be displayed; force with `--ascii`.
- No emoji.

### 10.4 Layout

- Tables: aligned columns with a header row and no box-drawing borders; wide tables drop the least important columns instead of wrapping.
- Units: KiB, MiB, GiB; durations like `2m 14s`.
- Times: local time with zone abbreviation in human output (`14:03:12 WAT`); RFC 3339 UTC in `--json`.
- Progress: one updating stderr line in a terminal; occasional log lines otherwise; a JSON event stream with `--json`.
- Every failure or warning ends with the next command on its own `→` line, in bold.

### 10.5 Example

```
TARGET    ENGINE       AGENT              PITR WINDOW                LAST VERIFY
prod-pg   postgres 17  ✓ capturing (4s)   7d 0h · up to 4s ago      ✓ standard, 3d ago
prod-mg   mongodb 8.0  ! stale (2h 14m)   6d 22h · up to 14:02 WAT  ✓ quick, 1d ago

! prod-mg: agent last seen 2h 14m ago; changes since 14:02 WAT are not protected
→ pentym catch-up prod-mg
```

### 10.6 Verbosity

`-q` prints only results and errors; the default is as above; `-v` adds detail such as per-step timings and message IDs; `--debug` prints the structured debug log to stderr. Verbosity never changes the format of stdout.

## 11. Message writing

Every error, warning, alert and prompt answers three questions in order: what happened, does it matter, and what do I do now.

### 11.1 Structure

```
<symbol> <target>: <what happened, with the specific fact>
  <impact on data or recovery>                (if not obvious)
  cause: <underlying error, scrubbed>          (when there is one)
→ <the next command or action>
```

### 11.2 Rules

1. Lead with the target and the fact, not the category.
2. Be specific: times with zones, positions, sizes, versions, durations. No "something went wrong" or "an error occurred".
3. State the impact on recovery whenever there is one.
4. Always give a next step; if none exists, point to the message's troubleshooting entry.
5. No blame, no "sorry", no "please".
6. Plain words; internal terms only when the reader can act on them, otherwise behind `-v`.
7. Never a secret, never row data.
8. Usually 2–4 lines; stack traces and full driver output only with `-v` or `--debug`.
9. Start lowercase after the target; no trailing full stop on single lines.
10. **Message IDs** (below).

### 11.3 Message IDs

- Format `PTM-` plus a number (for example `PTM-1203`), printed with `-v` and in `--json`.
- **Scoped:** only errors and warnings people act on — connection, privilege, gaps, verify, keys, storage — a few dozen in v1.
- One registry file in the code (ID, short name, docs link); each ID has a troubleshooting entry (cause, impact, fix) at `docs/errors/<ID>`.
- IDs are permanent: never reused, never changed in meaning; retired IDs stay reserved. A CI check enforces a docs entry for every ID and no reuse.

### 11.4 Examples

```
✗ prod-pg: cannot connect to 10.0.0.5:5432
  cause: password authentication failed for user "pentym_backup"
→ check the password reference for prod-pg.backup in config, then run: pentym doctor prod-pg
```

```
! prod-pg: role pentym_repl is a superuser; it only needs REPLICATION
→ pentym init --print-sql   (prints the minimal role definition)
```

```
✗ prod-pg: WAL missing from 14:10 to 14:52 WAT (slot invalidated by max_slot_wal_keep_size)
  point-in-time recovery currently covers only points after 14:52 WAT
→ pentym backup prod-pg   (starts a fresh base)
```

```
✗ prod-mg: verify (standard) failed at L3: restore reached 13:58:40 WAT, repository claims 14:02:11 WAT
  this snapshot was not marked verified
→ pentym check prod-mg, then see: pentym history prod-mg --since 1d
```

```
✗ prod-pg: DROP TABLE public.orders · xid 8812 · 14:03:12 WAT
  last safe point 14:03:11 WAT · PITR coverage OK
→ pentym restore prod-pg --before-event 3
```

### 11.5 Confirmation prompts

```
prune will delete 4 snapshots and 312 log segments from prod-pg, freeing 18.4 GiB
the newest successful and newest verified snapshots are kept
continue? [y/N]
```

The default is No. Rewinding a whole cluster requires typing the target name.

### 11.6 Never written

"Oops", "Uh oh", exclamation marks, "Something went wrong", "Please try again later" without saying what to change, "Contact your administrator" when the fix is known, and promises such as "your data is safe".

## 12. Docs style and glossary

The docs are split by purpose, written in plain second person, and use exactly one name for each concept.

### 12.1 Structure (Diátaxis)

| Kind | Purpose | Examples |
| --- | --- | --- |
| Tutorials | Learn by doing | Quick start (first backup in under 5 minutes); Undo your first DROP |
| How-to guides | Solve one task | Set up S3 or R2; run the agent on Kubernetes; verify in GitHub Actions; rotate keys |
| Reference | Exact facts | Commands and flags, config schema, metrics, message IDs, defaults |
| Explanation | Understand why | Architecture, repository format, crypto design and threat model, event detection |

### 12.2 Writing rules

- Second person, present tense; the task first, then the explanation.
- Short sentences and paragraphs; steps as numbered lists.
- Every command is copyable as is: no `$` prompt; placeholders like `<target>`, explained below the block.
- Platform tabs (Linux, macOS, Windows) where steps differ.
- Name the engine and version wherever behaviour differs.
- Every feature page ends with a **Limitations** section.
- Defaults are given with their value and a link to the defaults reference.
- Section 5's tone rules and banned words apply.

### 12.3 Glossary

| Term | Meaning | Don't call it |
| --- | --- | --- |
| Target | One configured database (Postgres cluster or MongoDB replica set) | source, instance, database connection |
| Repository | Where Pentymento stores everything (local folder or bucket) | vault, store, backup location |
| Snapshot | A base backup in the repository | full backup, dump, backup set |
| Log segment | One stored unit of WAL or one oplog batch | archive, log file, incremental |
| Chunk / pack / index | Deduplicated pieces of snapshots, grouped into packs, located via the index | block, blob |
| Agent | The long-running `pentym agent` process | daemon, service (except the OS service it runs as) |
| Lease | The agent's claim to capture a target | lock |
| Heartbeat | The agent's periodic liveness record | ping |
| Checkpoint | The last position safely stored | bookmark, cursor |
| Event | A detected destructive operation | incident, alert |
| Recovery window | The span of time you can restore to; configured and actual | retention, restore range |
| Gap | A missing stretch of log segments | hole, corruption |
| Anchor snapshot | The snapshot just before the recovery window begins, always kept | base, root |
| Side instance | The temporary database used by restore and verify | scratch DB, sandbox, temp server |
| Extract | Copy recovered objects from the side instance into production | import, merge |
| Rewind | Restore the whole cluster to a point (opt-in) | rollback, revert |
| Key slot | A way to unlock the repository's key; full slot or agent slot | password, key file |
| Journal | The repository's operations history | audit log, history file |
| Verify | Restore-based proof that a snapshot works, L0–L5 | test, validate |
| Catch-up | One-shot drain of WAL or oplog retained while the agent was down | sync, resync |

The same terms are used in CLI output, metrics, the PRD and the design document. A new feature that needs a new term adds it here first.

## 13. Presence

The project lives on its author's GitHub account under its own name, with `pentymento.dev` for docs and email.

| Place | Name |
| --- | --- |
| Repository | `github.com/HakeemTheEmperor/pentymento` |
| Go module / import path | `github.com/HakeemTheEmperor/pentymento` (GitHub path for now) |
| Install | `go install github.com/HakeemTheEmperor/pentymento/cmd/pentym@latest` |
| Domain | `pentymento.dev` — docs site (GitHub Pages) and email |
| Security contact | `security@pentymento.dev` (named in `SECURITY.md`) |
| General contact | A contact address at `pentymento.dev`, forwarded to the maintainer |
| Container image | `ghcr.io/hakeemtheemperor/pentymento` |
| Homebrew | Tap `HakeemTheEmperor/homebrew-tap` → `brew install hakeemtheemperor/tap/pentym` |
| Scoop | Bucket `HakeemTheEmperor/scoop-bucket` → `scoop install pentym` |
| GitHub organization | `pentymento` reserved, even while the repository stays on the personal account |
| Social handles | Optional; reserve `pentymento` only on networks actually used |

- Because the core is under `internal/`, nobody imports Pentymento as a library; switching to a vanity import path later would change only the install command and docs, and old versions stay installable from the old path.
- Any switch must be decided and documented before it happens, with the old path noted in the README.

## 14. Asset kit

All brand assets live in `docs/brand/`, with SVG as the master for every file and PNGs exported from it, never edited by hand.

### 14.1 Logo files

| Asset | Variants | Formats | Used for |
| --- | --- | --- | --- |
| Mark (D1) | Colour on light, colour on dark, single-colour ink, single-colour white | SVG; PNG 512, 256, 128 px | Docs header, avatars, slides |
| Mark, small (simplified D1) | Colour, single-colour | SVG; PNG 64, 32, 16 px | Favicons, small UI |
| Lockup (mark + wordmark) | Horizontal; light and dark; single-colour | SVG, PNG | README header, docs site, social preview |
| Wordmark alone | Light and dark | SVG | Where the mark doesn't fit |

### 14.2 Web and repository assets

| Asset | Spec |
| --- | --- |
| Favicon | `favicon.svg`; `favicon.ico` (16 and 32 px); 180 px Apple touch icon |
| GitHub social preview | 1280 × 640 PNG, light and dark (light used by default) |
| README banner | Wide lockup with tagline, light and dark, switched with GitHub's `<picture>` support |
| Terminal theme | Colour definitions for the screenshot theme (section 9.2) |
| Demo recording scripts | Scripted recorder files for the two-act demo |

### 14.3 Supporting files

- `docs/brand/README.md`: a one-page summary — name rules, tagline, palette with hex values, fonts, logo do's and don'ts.
- Diagram sources in `docs/diagrams/`.
- Licence files for any fonts shipped.

### 14.4 Production

SVG masters are drafted from the D1 concept at the asset-building stage; the mark's final geometry (stroke weights, arrowhead, spacing) gets a careful dedicated pass.

## 15. Trademark usage

Anyone may use the code under Apache 2.0; the name and logo identify the official project only. This becomes `TRADEMARKS.md` and is a practical policy, not legal advice.

### 15.1 Status

Pentymento is an unregistered trademark: shown as Pentymento™ once, on the first prominent use (README header, docs home), never as ®. Registration is revisited only if adoption or a commercial side justifies it.

### 15.2 Allowed without asking

- Articles, tutorials, talks, videos, comparisons and reviews that name Pentymento, including critical ones.
- "Works with Pentymento" or "X for Pentymento" for integrations, dashboards and modules.
- Using the unmodified logo to link to or refer to the project.
- Packaging unmodified releases under the Pentymento name; packaging-only changes and security patches are fine if documented.
- Forking the code for any purpose.

### 15.3 Not allowed without permission

- Naming a fork or modified version Pentymento or anything confusingly similar; modified versions use a different name and preferably a different binary name than `pentym`.
- Implying endorsement or official status.
- Selling a service under the name ("Managed Pentymento", "Pentymento Cloud"); "hosting for Pentymento" is fine.
- Domains, package names or social accounts that could be mistaken for the official project.
- Modifying the logo or using it for a different product.

### 15.4 Forks

Forks keep the copyright notices and `NOTICE` file (Apache 2.0 §4) and may say "based on Pentymento".

### 15.5 Contact

Questions and misuse reports go to the project contact address. Enforcement starts with a polite request.

## 16. Decision log

| # | Decision | Outcome |
| --- | --- | --- |
| 1 | Name usage | Pentymento in prose, `pentym` for anything typed; `pentym` identifier family; pronunciation stated |
| 2 | Tagline | "Undo the DROP." plus subtitle and metaphor line |
| 3 | Story of the name | One-line, paragraph and long-form versions; recoverable, never "nothing lost" |
| 4 | Messaging pillars | Undo the mistake (lead); backups you can prove; secure by design; one binary, no babysitting |
| 5 | Audience and tone | Self-hosters and small teams first; calm, precise, honest, helpful |
| 6 | Logo | D1: counter-clockwise arc around three even layers; monochrome by design (chosen over the ghost-letter, offset-layers and underpainting concepts and the D2/D3 refinements) |
| 7 | Colour | Indigo brand colour; conventional status colours; colour never the only signal |
| 8 | Typography | Inter and JetBrains Mono |
| 9 | Imagery | Real, flat, labelled; no stock, 3D, AI art or painting props |
| 10 | CLI output | Words first; stdout/stderr split; ANSI colours on TTY only; ASCII fallback; no emoji |
| 11 | Message writing | What happened, impact, next step; scoped `PTM-` message IDs in v1 |
| 12 | Docs style | Diátaxis structure; Limitations sections; single glossary |
| 13 | Presence | GitHub import path for now; `pentymento.dev`; ghcr, Homebrew tap, Scoop bucket; `pentymento` org reserved |
| 14 | Asset kit | SVG masters in `docs/brand/`; logo, favicon, social preview, banner, terminal theme, demo scripts |
| 15 | Trademark | Unregistered ™; forks renamed; "for/with Pentymento" allowed |
