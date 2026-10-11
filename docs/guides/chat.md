# Chat and models

How a conversation works in ws, how a model gets chosen, and what happens to long conversations. For the commands to get ws running, see [Start here](../START-HERE.md).

## Projects and conversations

A **project** holds conversations. The sidebar's "New conversation" starts a chat in your most recently active chat project (never a code or design project, whose conversations open their panels), or creates one called "General" if you have none; a project can also be a [code project](code-projects.md), which adds a sandbox. Every turn is an agent run: one or more model calls, plus any tool calls the model asks for. Runs are saved as they go, so they survive a restart of the server; a run whose process stopped is picked up and finished by the worker. Hover a conversation in the sidebar to rename it or archive it (or double-click its name to rename): renaming edits the name in place, where Enter saves, Escape cancels and leaving the field saves, and archiving asks you to confirm. Archived chats wait in **Settings, Archive**: **Restore** puts one back; **Delete permanently** removes its messages, files and artifacts and frees the space (the dialog says how much, and it cannot be undone), and **Delete all archived** does it for every archived chat at once. Memories ws kept from a deleted chat stay, and so does the spend history. The same page lists your projects: **Archive project** hides a project and its chats (restore brings them back), and only an archived project can be deleted permanently, by typing its name; that also removes its agents, its sandbox and its files. The sidebar also links to Media and Settings, and a **More** menu at its bottom holds the other pages (see [On a phone](#on-a-phone)).

## Choosing a model

The model picker has three groups:

- **Routing:** aliases defined in your policy files (the shipped ones are `best`, `cheap`, `local` and `code`), plus the built-in `auto`, which is the default. A routing name lets ws pick the endpoint for each request from your policies, filtered by what the request needs (tools, images) and by which endpoints are healthy.
- **Local:** endpoints on your own hardware.
- **Hosted:** endpoints from providers such as Anthropic, OpenAI and OpenRouter.

An endpoint that is down is disabled in the picker, and a local one also shows "(down)"; image and video models are not listed (they are used through Images and `generate_image`); an endpoint an admin has disabled is not listed.

The brain icon next to the picker asks for extended thinking. It has an effect only on Anthropic-protocol endpoints that support it; local and OpenRouter endpoints ignore it today. It is not remembered and is off again when you reload the page. Which endpoints exist, and what each can do, comes from `config/endpoints.yaml` and from the Admin page; see [Admin](admin.md) and [Configuration](../CONFIGURATION.md).

## Attachments

"Attach" accepts images, PDF, plain text, Markdown, JSON and CSV files. What the model sees depends on the endpoint. The router only filters on images (it picks a vision-capable endpoint) and on tools. A PDF is read only by Anthropic-protocol endpoints; an OpenAI-compatible endpoint gets a short placeholder saying the file was omitted, and text and JSON files are included as text there. A file whose type the browser leaves blank can be omitted the same way.

## Tools in chat

A chat turn can call these built-in tools: `create_artifact` and `update_artifact` (see [Artifacts](artifacts.md)), `web_fetch` (public URLs only; private, loopback and tailnet addresses are refused), `read_blob` (to page through a long tool result that was stored outside the conversation), `ws_help` (ws's own documentation, the pages of this docs site as they were when the running build was made, so you can ask the assistant how ws works and it answers from these pages and names the one it used) and `ask_user`. When the model calls `ask_user` it stops there: the chat shows the question in a card headed "A question for you", the turn is over, and you answer in the message box like any other message. When an image model is configured it can also call `generate_image` (the results appear in the chat; the Media page uses the same models), and when the operator turns task dispatch on it can call `spawn_job`. Tools from MCP servers listed in `config/mcp.yaml` appear as `mcp__<server>__<tool>`. Code projects are offered every tool the worker has, including these and the sandbox tools. Each tool has a policy that decides whether it runs on its own or asks you first; see [Approvals](approvals.md).

**Documents.** Attach a PDF, Word, Excel, PowerPoint or text file and ws reads it before the model does: the message carries the text with page, sheet or slide markers, up to about 12,000 characters, and the model can call `read_document` for the rest or search it. The original is kept as an attachment. PDFs also go to models that read them natively. A scan or a photographed page is read by OCR when the operator has set up the OCR container; without it a scanned PDF is reported as unreadable and says why.

**Shortcuts and favorites.** Type `/` in the message box for a list you can move through with the arrow keys: `/plan` (plan first), `/remember something` (keep it in your memory), `/schedule` (open Scheduled with the text filled in), `/notebook` (open your notebooks), and your saved prompts, which insert their text. Escape closes the list. The star under an answer, in an artifact's header and on a media item adds it to Favorites, one list across your projects; saved prompts are written on the same page.

**Notebooks.** A notebook (in the sidebar's **More** menu) collects sources for one piece of research: files, a public web page, pasted text, or an earlier conversation. Each is read and indexed. "Open chat" starts a chat whose context is the notebook: replies cite their sources as small links that jump to the passage, a dotted underline on a word shows a short explanation on hover, and the model keeps a summary and a list of open questions up to date on the notebook page, next to your own notes. A notebook is yours alone; the chat is an ordinary conversation, so it also appears in your list.

**Scheduled prompts.** The Scheduled page (in the sidebar's **More** menu) runs a prompt for you on its own, once at a time or on a repeat such as weekdays at 07:30 in your time zone. Each run is an ordinary chat in the project you pick, with no tools unless you allow reading public web pages, and its answer waits in a conversation (a new one each time, or one you keep adding to). The page lists each schedule with its next run, last result and history, and has Run now, Pause and Delete; an option emails you a link when a run finishes. A repeat may run at most every 15 minutes, you can keep 20, and your usual budgets apply.

**Memory.** ws can remember things about you between chats. Tell it something worth keeping ("I prefer metric units", "call me Sam") and it may keep it with `remember`; the next chats start with your card and the memories that match what you wrote, and a "from memory" line under the reply lists which ones were used. The Memory page (linked from Settings) shows every memory, lets you edit or delete any, edit the card, turn memory off, and switch on a nightly tidy-up that reads the day's chats, keeps what is new and merges repeats. Memory is yours alone and is not used for training.

**Plan first.** The list icon in the message box (or `/plan` at the start of a message, or Shift+Tab) makes the model write a plan before it does anything: a summary and numbered steps, with the tools each step needs. The plan appears as a card and the turn waits. Approve runs it; Edit lets you change the steps (one per line) and approve your version, which the model is told to follow instead of its own; Reject ends the turn, with an optional reason. Once approved the card is a checklist that ticks as the model reports each step done. Plan first applies to the message you send next and then switches off. An agent can be set to plan first on every run (`plan_first` in its model settings), so a scheduled run waits on its monitor page for your approval each time.

## Long conversations

ws never edits what is stored. When a request would be larger than the compaction budget (`WS_COMPACTION_BUDGET_TOKENS`, 60,000 tokens by default, lowered per request to the largest context window among the models your choice could route to, less room for the reply), it sends a stored summary of the older turns plus the recent turns instead, and drops the oldest turns for that one request if it is still too long. A background job writes a fresh summary with a cheap model. Your full history stays as it was. If a conversation still does not fit the model you picked (or, with `auto` or a routing alias, any model that choice could use), the chat says "This conversation is too long for the model. Start a new conversation, shorten the message, or pick a model with a larger context." and nothing else. The size is estimated before the call, so very number-heavy or non-English text counts for more than plain English.

## Rating answers

Opening a conversation (from the sidebar, New conversation, a link or a reload) puts the cursor in the message box on a desktop, so you can start typing at once; a key typed while nothing has the focus goes to the box and is kept. A phone is left alone, because focusing the box would raise the keyboard over the chat, and so is a field that already has the focus (the rename field, a dialog's field).

Each answer has a thumbs up and thumbs down; clicking the same one again clears it. Ratings are stored per user. They matter only if the owner builds a fine-tuning dataset, which also needs your consent: **Settings** has an "Allow my conversations in training datasets" box, off by default (see [Trust and security](../TRUST.md)).

## Cost and limits

Every model call is written to a usage ledger with its tokens, cost and the routing decision. The owner can set budgets that block a call or switch it to local endpoints once a limit is reached; see [Admin](admin.md).

## Your name

Settings is split into groups, listed down the left (a drop-down on a phone): **Profile** (display name, where ws sees you, and passkeys; the page opens here), **Provider keys**, **Training** and **API keys**. The open group is kept in the address (`/settings#api-keys`). The owner also sees the **Job reporting** option inside the API keys group; members do not see it at all. The profile group has a display name field and a **Save name** button. The name is the one thing about your account you edit yourself, 1 to 80 characters; your address and role are fixed. When the owner has told ws which networks are home (`WS_HOME_NETWORKS`), the sidebar shows a small **home** or **away** mark; tap it to see how ws decided. From away, the sensitive actions (saving a provider key, minting an API key, adding or removing a passkey, exporting, and the owner's Admin writes) ask for a **passkey confirmation** first: a dialog appears, you tap your passkey, and the action goes through; it then stays unlocked for ten minutes. An account with no passkey is refused from away, and the first passkey has to be added from home.

## On a phone

The bottom of the sidebar is one row: a menu icon and your name (your email shows when you hover it), and the whole row, including the empty space to the right of the name, is a single button. It opens a menu above it with Media, Settings, Agents, Favorites, Notebooks, Scheduled, Sign out and, for the owner only, Jobs, Training and Admin. It closes on Escape (focus goes back to the button), on a click outside, on Tab out of it and when you pick a page; Tab and Enter work in it. On a phone it is the same menu inside the drawer.

Below 768 px wide the sidebar becomes a drawer that opens from a top bar, the artifact, code and design panels open over the page instead of beside it, and the agent monitor and Jobs panes stack. The page should never scroll sideways. This is read from the layout code (#158) and its browser tests; it has not been tried on a real phone.

*Checked against the code at master `13a673a`.*
