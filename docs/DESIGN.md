# Design

The visual system lives in the **ws** Design System artifact: https://claude.ai/artifact/SHvCNWYRv9D64Km7kjYnqV. Its brand book (`project/README.md` inside that artifact, not in this repo) and its `project/tokens.json` are the source of truth; this file is the short version for people editing `web/`.

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

## Type

- `.reading` 17px Georgia, 1.65 leading: replies, user messages, the composer.
- `.page-title` 27.2px regular. Headings never go bold.
- `.section-head` small caps, lowercase text, `fg-3`, `line-soft` rule. Use `<SectionHead>`.
- `.meta` and `.tagline` italic serif for meta lines and empty states.
- Interface: `text-base` inputs and buttons, `text-sm` rows and controls, `text-xs` chips and captions, `text-[11px]` hints. Weight 400, 500 for buttons. `font-mono` for ids, keys, JSON.

## Shape

`rounded-md` small controls, `rounded-lg` buttons, inputs, lists, callouts, `rounded-2xl` only message bubbles and the composer, `rounded-full` only send and stop. Code uses `--radius-code` (2px). No shadows, no gradients, no entrance animations.

## Components

Shared primitives are in `web/src/components/ui.tsx`: `SectionHead`, `PageTitle`, `Divider`, `Callout`, `ListGroup`, `Row`, and the `btn` / `input` class sets. Everything else is Tailwind utilities in the TSX.

## Copy

Lowercase **ws**. Sentence case. Short second-person sentences with periods. Real ellipsis in progress copy ("Thinking…"). Italic meta joined by middle dots ("routed to orin · 3.2 s"). No emoji, no exclamation marks.

## Not yet built

The design system also specifies a Jellyfin music feature: `MiniPlayer` and `Queue` docked in the sidebar, `TrackList` and `AlbumCard` rendered inside replies from a `music_search` tool, with the Go server proxying Jellyfin and holding its API key. That needs a backend and is not scheduled in a milestone yet.
