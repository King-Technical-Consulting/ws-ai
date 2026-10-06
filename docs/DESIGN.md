# Design

This file is the source of truth for the visual system. It supersedes the earlier **ws** Design System artifact (https://claude.ai/artifact/SHvCNWYRv9D64Km7kjYnqV), which is kept only for history; Jeremy made this change on 2026-10-06 when the branding and logo package arrived. Tokens are declared in `web/src/index.css`; this file is the short version for people editing `web/`.

## Voice

ws should feel like jeremyking.co grew a chat window: warm paper, dark-brown ink, one burnt-orange accent, Georgia for anything you read, small-caps section heads, dotted link underlines, hairlines instead of shadows. Dense controls stay in the system sans.

## Tokens

Declared once in `web/src/index.css` under `@theme` (light) and `prefers-color-scheme: dark`. Use them as Tailwind utilities, never raw colors or Tailwind's palette.

| Token | Use |
|---|---|
| `bg` | page ground |
| `bg-2` | raised paper: sidebar, inputs, composer, callouts |
| `bg-3` | hover, active rows, user bubbles, chips |
| `fg` | content text |
| `ink-2` | emphasized secondary text: italic subheads, quotes |
| `fg-2` | helper copy, inactive nav |
| `fg-3` | small-caps heads, italic meta (3.7:1 in light: never body text) |
| `fg-4` | placeholders and decoration only |
| `line` / `line-soft` | structural hairlines / quiet rules |
| `accent` | the one hue: links, primary buttons, send, focus |
| `accent-hover`, `accent-strong`, `accent-fg` | link hover, filled-button hover and visited, text on accent |
| `link-underline` | dotted underline under links at rest |
| `danger` | errors and destructive text links, always with words |
| `ok` | olive: healthy, local endpoints. Outlined chips, badges and diagrams only |
| `info` | slate: informational, hosted endpoints, queued. Same limits as `ok` |

`ok` and `info` are never buttons or links, and never filled: orange stays the only interactive hue. Light and dark values are in `web/src/index.css` (`ok` #5f6a3a / #a9b36f, `info` #4f6272 / #8ea6b8).

## Type

- `.reading` 17px Georgia, 1.65 leading: replies, user messages, the composer.
- `.page-title` 27.2px regular. Headings never go bold.
- `.section-head` small caps, lowercase text, `fg-3`, `line-soft` rule. Use `<SectionHead>`.
- `.meta` and `.tagline` italic serif for meta lines and empty states.
- Interface: `text-base` inputs and buttons, `text-sm` rows and controls, `text-xs` chips and captions, `text-[11px]` hints. Weight 400, 500 for buttons. `font-mono` for ids, keys, JSON.

## Shape

`rounded-md` small controls, `rounded-lg` buttons, inputs, lists, callouts, `rounded-2xl` only message bubbles and the composer, `rounded-full` only send and stop. Code uses `--radius-code` (2px). No shadows, no gradients, no entrance animations.

## Logo

The routing mark: one line leaves a start dot and splits in two. The filled accent dot is a hosted model, the accent ring is a local one. Use `<WsMark />` from `ui.tsx`; ink follows `currentColor` and the dots use `accent`, so it works in both themes. Never hard-code its colors in the app.

- Beside the small-caps `wordmark` in the sidebar, and on Login and Invite. Static everywhere except Login, where `draw` plays the draw-on once.
- Lockups: mark plus "ws" in Georgia with a short accent rule beneath (horizontal), or the mark over small-caps "ws" (`ink-2`, .3em tracking) and the italic tagline "a self-hosted AI workspace" (stacked).
- Clear space is the height of the filled dot on every side; minimum size 16px. Below 32px the component thickens its strokes.
- Favicon (`web/public/favicon.svg`): white mark, thick stroke, no dots, on an accent tile, so it holds at 16px. Apple touch icon (`web/public/apple-touch-icon.png`, 180px): cream mark with orange dots on a #2c2416 tile, full bleed because iOS rounds the corners itself.
- README banner: `public-repo/assets/banner.svg`. Its tagline is lowercase, like the stacked lockup's: "a self-hosted AI workspace: your models, your machines, one login."
- The monogram options explored in the brand file (serif ws, small caps in a double frame, two linked nodes) are not adopted. Ask Jeremy before using one.

## Status chips

`<StatusChip kind="ok" | "info">`: 12px sans, 1px outline in the hue, `rounded-md`, 10px by 5px padding. Never clickable.

## Motion

Logo draw-on on Login only: trunk 1.6s ease-out, second branch .2s later, destination dots fade in at 1.5s and 1.7s. Page content may fade in over 300ms. The "Thinking…" pulse stays. Nothing else moves. `prefers-reduced-motion` disables the draw-on.

## Components

Shared primitives are in `web/src/components/ui.tsx`: `SectionHead`, `PageTitle`, `Divider`, `Callout`, `ListGroup`, `Row`, `WsMark`, `StatusChip`, the `btn` / `input` / `inputSm` class sets, and in `web/src/index.css` the `.reading-tight`, `.prose-ws` and `.wordmark` classes. Everything else is Tailwind utilities in the TSX.

## Copy

Lowercase **ws**. Sentence case. Short second-person sentences with periods. Real ellipsis in progress copy ("Thinking…"). Italic meta joined by middle dots ("routed to orin · 3.2 s"). No emoji, no exclamation marks.

## Not yet built

The design system also specifies a Jellyfin music feature: `MiniPlayer` and `Queue` docked in the sidebar, `TrackList` and `AlbumCard` rendered inside replies from a `music_search` tool, with the Go server proxying Jellyfin and holding its API key. That needs a backend and is not scheduled in a milestone yet.
