# Design system

The terminal's visual rules, and the measurements behind them.

Two reference interfaces were captured at 1440×900 in a real browser and their
computed styles sampled across every element. They are recorded here because
they disagree with each other almost completely, and the disagreement is what
makes them useful: each is right for a different kind of surface, and this
terminal has both kinds.

Neither is a target to copy. Both get one thing wrong that matters more here
than anything they get right -- see "Where both references are wrong".

## What was measured

### Reference A -- a cryptocurrency exchange terminal

Dark mode, 1226 elements sampled.

| Property | Value |
|---|---|
| Page surface | `#131722`, **95.3%** of painted area |
| Panel surface | `#1C2030`, 2.4% |
| Raised / border | `#2A2E39`, 1.7% |
| Up / down | `#26DE81` / `#FF231F` -- **identical in light and dark** |
| Text | `#FFFFFF` → `#C5CBCE` → `#758696` → `#4F5966` |
| Family | Overpass, **528 of 529** text nodes |
| Size | **13px on 406 of 529 nodes.** Then 14px (87), 16px (25), 12px (6), 11px (5) |
| Weight | 400 on 476 nodes. 300 on 48. **700 on five** |
| Radius | 5px (32), split radii for grouped controls (17+17), 4px (12), 2px (6) |
| Border | `1px` hairline, **504 occurrences** |
| Padding | `10px 18px`, **477 occurrences** |
| Shadow | **Two, and only two:** a green glow on BUY and a red glow on SELL |
| Height | 1463px at a 900px viewport -- barely scrolls |

Three things stand out.

**Hierarchy is carried by colour, not by type.** One family, one size on 77% of
nodes, one weight on 90%. Bold appears five times on the entire page. A reader
finds the important number because it is green or red, not because it is bigger.

**Structure is lines, not elevation.** 504 hairlines and zero elevation
shadows. Nothing floats above anything.

**Shadow is reserved for irreversibility.** The only two shadows in the
document are coloured glows on the two buttons that spend money. Shadow is not
decoration there; it is a category marker for "this action cannot be undone".

### Reference B -- a shadcn admin dashboard

Dark mode, same viewport.

| Property | Value |
|---|---|
| Page surface | `oklch(0.145 0 0)`, 51.1% of painted area |
| Card surface | `oklch(0.205 0 0)`, **47.3%** |
| Border | `oklch(1 0 0 / 10%)` -- white at 10%, not a colour |
| Family | Inter |
| Size | 14px (80) and 12px (80) **equally**, 16px (26), 24px (8), 30px (1) |
| Weight | 400 (111), 500 (60), 600 (15), 700 (9) -- four levels, all used |
| Radius | 8px (60), 10px (21), 14px (11), pill (25) |
| Gap | 8px (64), 4px (33), 6px (24), 24px (17) -- a 4px scale |
| Grid | 4 × 266px at 16px gap; 2 × 544px at 24px gap |
| Shadow | `0 1px 2px rgba(0,0,0,.05)` and `0 1px 3px rgba(0,0,0,.1)` -- real elevation |

Nearly half the painted surface is a raised card. Type hierarchy is real and
four levels deep. Spacing is a strict 4px scale. This is the opposite of
Reference A on every axis.

## What the disagreement means

Reference A is right for a surface where the operator is **reading many numbers
at once and acting on them**. Density is the feature; a card border around each
number would push the second half of the book below the fold.

Reference B is right for a surface where the operator is **being told
something** -- a few figures, each needing a label, a delta and a sentence of
context. Density there is a liability; the reader needs to be walked through it.

This terminal has both. So it uses both, and the rule is which surface you are
on:

| Surface | Rule | Pages |
|---|---|---|
| **Trading** -- reading and acting | Reference A. 13px, one weight, hairlines, no cards, no elevation, colour carries hierarchy, maximum rows above the fold | trade, orders, positions, portfolio, markets, operations |
| **Analytical** -- being told something | Reference B. Cards on a 4px scale, 4-up then 2-up, a delta badge, one plain sentence per figure, real type hierarchy | overview, strategies, backtests, ml, scanner, calendar |

A page does not mix them. The boundary is the page, not the panel.

## Where both references are wrong

**Neither renders money correctly.** Reference A puts prices in Overpass, a
proportional face: the decimal points do not align down a column and the digits
change width as they update, so a price column visibly jitters on every tick.
Reference B has no money in it at all.

This platform's rule is stricter than either, and it is not aesthetic. Money is
a decimal string from the API to the screen and is never parsed into a float
(see `lib/format.ts`). It is rendered in a monospace face with `tabular-nums`,
so a column of prices has its decimal points on one vertical line and a digit
changing from 1 to 8 does not reflow the row.

A trading terminal whose numbers jitter is worse than a plain table. This is
the one place to beat both references rather than follow them.

**Neither has a mode marker.** Every figure in this terminal is simulated. The
PAPER marker is not a badge in the corner that a confident reader learns to
stop seeing -- it is in the top bar, on the account, and beside any figure that
could be mistaken for real money.

## The tokens

Defined in `app/globals.css`. The surface ramp is deliberately darker and
flatter than Reference A: this terminal runs for hours on a single screen and
the panel-to-page step only has to be visible, not decorative.

Green and red mean profit and loss and nothing else -- never "primary button",
never "success toast". That is why the primary accent is gold: an action needs
emphasis that cannot be read as a gain.

### Shadow

There are exactly two elevation shadows in this terminal, and they are the
coloured glows on **confirm buy** and **confirm sell**. That is Reference A's
idea and it survives review here because it says something true: those are the
only two controls that place an order, and an order cannot be unplaced. Every
other surface is separated by a hairline.

Adding a third shadow means claiming a third thing is irreversible. Do not.

### Type

13px base, `tabular-nums` on every numeric cell, monospace for money and for
any identifier a person might read out loud.

On analytical surfaces the scale opens to 12/13/16/24 with weights 400/500/600.
On trading surfaces it stays at 13/400 and colour does the work.

## What the tests pin

`apps/web/tests/e2e` asserts structure, not appearance, and a redesign must
keep these or change them deliberately:

- `.topbar` and `.login-card` -- the two selectors the sign-in helper waits for.
  Renaming either makes all 57 tests fail at sign-in, and the failure reads as
  a broken login rather than a renamed class.
- Machine-readable refusal codes. The terminal holds no business rules; every
  refusal comes from the API with a code, and the tests assert the code rather
  than the prose. Prose may be rewritten freely; the code may not be hidden.
- The PAPER marker, asserted on the terminal and on the chart.

## Method

Captured with a real browser at 1440×900: computed styles sampled across every
element, backgrounds weighted by painted area so a colour that appears in the
DOM but covers nothing is not mistaken for a brand colour, and each reference
screenshotted in both themes. Percentages above are share of painted area;
counts are occurrences across the sampled elements.
