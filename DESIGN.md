# Design

<!-- impeccable:design-schema 1 -->

Recorded from the built artifact, `ui.html`, not from intentions. One file, no build step,
no network at runtime: the page is `go:embed`ed into the binary and served on a local
network that need not reach the internet. Every rule below exists because the page already
follows it.

## Surface

One surface: the alert settings editor at `SKY_LISTEN`. Visitor mode is **Operate** — the
operator is completing a task, and the dominant task is auditing the whole rule set.

## The idea the surface owns

A rule is an allowlist, so a rule that matches nothing costs every alert it was meant to
catch, and the YAML looks perfect either way. Coverage is therefore permanent furniture,
never something you request: every rule carries its figure in the list, and the selected
rule leads with it before its conditions.

The second half of that idea is that the figure has to be honest. Where a database row
could not both narrow the rule and satisfy it, the page shows `—` and the reason rather
than a number: a `squawk`-only rule would otherwise read as the whole database, and a
`listed: false` rule as zero. Both are lies dressed as figures.

## Color

Cool-biased neutrals — the greys carry a slight blue, toward the accent, rather than being
pure. One accent, used only for selection, the primary action and a chosen value. Semantic
amber and red are separate from it and never used decoratively.

Strategy: **Restrained.** The visitor came to operate.

| Token | Light | Dark | Used for |
|---|---|---|---|
| `--bg` | `#F5F6F9` | `#0C0F13` | page ground |
| `--panel` | `#FFF` | `#151A20` | the two panels, condition cards, popovers |
| `--sunk` | `#EFF1F5` | `#1C222A` | the result band, hover, disabled fields |
| `--line` | `#E1E4EB` | `#262D36` | hairlines between rows, bands and cells |
| `--line2` | `#C9D0DA` | `#39424E` | input and button borders |
| `--fg` | `#12161C` | `#E7ECF2` | body |
| `--fg2` | `#57616F` | `#98A3B1` | labels, help text |
| `--fg3` | `#828D9C` | `#6B7686` | counts, placeholders, row numbers |
| `--acc` | `#2B55C8` | `#88A6FF` | selection, Save, a chosen value |
| `--acc-soft` | `#E9EEFB` | `#1A2440` | selected row, selected chip |
| `--warn` | `#8A4A0B` | `#E4B173` | conditions the preview cannot see; unreachable rules |
| `--bad` | `#A8261E` | `#F09189` | a rule that can never alert; a value that matches nothing |
| `--good` | `#0F6B3F` | `#62C490` | a completed save |

Three theme states, not two. The bare `:root` block defines the complete light palette;
`@media (prefers-color-scheme: dark)` redefines only the tokens, guarded as
`:root:not([data-theme=light])`; `:root[data-theme=dark]` redefines them again so the
toggle wins in both directions. No colour is ever named outside those three blocks. The
toggle writes to `localStorage` only on a deliberate click, so the default is the system's.

## Type

**Public Sans**, latin subset, variable 400–700, embedded as a base64 `woff2` (~26 KB).
Embedded rather than linked: a font that silently falls back is a different design, and
this page is served on a network that need not have internet access. SIL OFL 1.1. The
fallback stack is `system-ui, -apple-system, Segoe UI, sans-serif`.

`ui-monospace` appears in exactly one place: the cooldown fingerprint of an unnamed rule,
because it is a hash and reads as one.

| Role | Size | Weight | Notes |
|---|---|---|---|
| Body | 14.5px / 1.55 | 400 | `tabular-nums` throughout |
| Coverage figure | 32px | 600 | `-0.025em`, the largest thing on the page |
| Pane heading | 17px | 600 | `-0.012em` |
| Toolbar / list heading | 16 / 14.5px | 600 | |
| Rule name in list | 14px | 600 | |
| Group and field labels (`.lbl`) | 11.5px | 650 | uppercase, `0.055em` |
| Help and caveat text (`.help`) | 12.5px / 1.5 | 400 | `--fg2`, `max-width: 72ch` |
| Table | 13px | 400 | headers are `.lbl` |

## Layout

Two panels on a 340px + fluid grid, max 1280px, 16px gutter. The rule list is sticky at
`top: 72px`; the detail pane scrolls. Below 900px the two become one column and the panels
swap by `body[data-view]`, with a back control in the pane header. Below 600px each group
header stacks its Add control under the heading.

Spacing is on a 4px base: 6–7px inside chips, 10–12px inside cards, 14–18px for band
padding, 16px between panels. Radii: 9px for panels, cards and buttons; 6px for inputs and
inner boxes; 99px for chips and badges. Shadows only on the two panels and the popover —
every other separation is a 1px hairline. The result band is a sunk fill, not a card;
nested cards do not exist on this page.

## Components

- **Rule row** — `[number] [name or fingerprint / sentence] [coverage badge]`. The name is
  deliberately quiet: unlabelled rules show the server's fingerprint in mono grey, because
  the sentence beneath is what identifies the rule.
- **Coverage badge / figure** — neutral for a count, amber for the wildcard, grey for
  unpreviewable, red for a rule that can never alert.
- **Condition card** — a hairline box with an uppercase label, its values, and its caveat.
  Amber fill when the preview cannot see it; red fill when it would be rejected on save.
- **Chip** — one value. Carries the number of aircraft it covers, or `not found` /
  `feed only` / `clashes` when it covers none, each with the reason in a `title`.
- **Toggle chip** — a fixed option set (operator class). Dashed when off, accent when on,
  `aria-pressed` either way.
- **Popover picker** — one at a time, closed by Escape or a click outside. Renders 150 rows
  per batch as it is scrolled, ranks prefix matches first, and states how many values it is
  showing and what narrowed them.
- **Result band** — figure, sentence, the not-counted line, then the matching aircraft in a
  table that scrolls horizontally in its own container rather than breaking at phone width.

## Copy

The product's own voice, taken from the README: plain, direct, and willing to state the
limitation in the same breath as the feature. Every caveat is where it applies, not in a
legend. No exclamation marks, no emoji, no marketing register. Controls name their action
(`Save`, then `Saved`). Errors name the problem and the recovery.

Sentences the page generates — "Police aircraft, within 15 NM of the receiver." — are built
from the rule itself, so they cannot drift from what will be saved.

## Rules the build keeps

1. **Nothing derives what the server owns.** The cooldown fingerprint comes from
   `/api/preview` and `/api/alerts`; the page never computes its own, because a key
   invented here would be a different string from the one in the ledger and the logs.
2. **Read the payload, not the DOM.** The sentence, the coverage figure and the wildcard
   warning are all computed from the rule as it will be written, so the preview can never
   answer a different question than the save.
3. **Typing never rebuilds the pane.** A structural change re-renders; a keystroke in a
   number field moves only the figure, the sentence and the list row, so the caret stays.
4. **Per-rule state is keyed by the rule object**, not its position, so reordering and
   deleting carry it with no bookkeeping.
5. **Browser surfaces are themed**: selection, focus ring, scrollbars, `color-scheme`, and
   tabular figures wherever digits line up.
6. **Motion is transitions only** — background, border and colour at 120–140ms — and
   `prefers-reduced-motion` removes all of it. There is no entrance animation; the page is
   complete at rest.

## Anti-patterns for this surface

No cards inside cards. No coloured left borders. No icon-plus-heading grids. No monospace
as a costume for "technical" — only the one hash. No emoji standing in for icons; the eight
icons are authored SVG on one 1.6px round-cap stroke. No gradient text, no glass.
