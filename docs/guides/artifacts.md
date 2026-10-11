# Artifacts

An artifact is a document the assistant produces next to the conversation, such as a web page, a diagram or a file, that you can open, read and revise.

## What it can be

The assistant creates one with `create_artifact` and changes it with `update_artifact`. Each artifact has one of these kinds:

| Kind | What it is |
|---|---|
| `html` | A web page, rendered live |
| `svg` | A vector image |
| `markdown` | A formatted document |
| `mermaid` | A diagram written in Mermaid syntax |
| `code` | A source file, shown as code |
| `design` | An HTML mockup made in a design project (see below) |

A `design` artifact is an HTML mockup made in a design project: the conversation's design system (the palette button in the chat header: library, colors, type, spacing, radius, components, notes) goes into the assistant's instructions and is stored on each version as its `design_context`. The panel shows the system's swatches, a diff against the previous version, a download of any version, and a variants button that asks the model for up to four alternatives of the current version, each stored as its own design in the conversation. React components are planned and are not rendered yet.

## Versions

The chat header has an **Artifacts** button (the panel icon, shown when the conversation has at least one artifact): it opens a list of the conversation's artifacts, newest first, with their kind and newest version, and a tap on one opens it; the back arrow in the artifact's header returns to the list. A code conversation also has a **Sandbox** button (the terminal icon) for its files, terminal and preview. Only one right-hand panel is open at a time: opening one closes the other. The artifact panel is closed when you open a conversation, even one that holds artifacts; click an artifact's card in the message to open it. An artifact that arrives while you are in the conversation opens the panel by itself on a screen wide enough to show it beside the chat (768 px and up); on a phone it does not, because the panel would cover the chat, so tap the card.

Every change makes a new version, and the earlier ones stay. The panel shows the artifact with its version number, and you can open an earlier version. The API returns the full list; see [HTTP API](../API.md).

## Where it runs

Artifacts are rendered on a separate origin (`WS_ARTIFACT_URL`, port 8081 locally) inside an `<iframe sandbox="allow-scripts">`. The exception is the `code` kind, which the app shows as plain text in the page and does not run. The page can run its own scripts, but it cannot read the app's cookies or storage, cannot make network requests, and cannot load images from the network, so a generated page cannot leak the conversation. Scripts may come from a short list of CDNs (cdnjs, jsDelivr, the Tailwind CDN and unpkg); stylesheets from cdnjs, jsDelivr and Google Fonts, and font files from cdnjs and Google Fonts. Web links (`http` and `https`) inside an artifact open in a new tab through the parent page. The links the app uses to show an artifact are signed and expire after 24 hours by default, and a single version is limited to 2 MiB.

Do not point the app and artifact hostnames at the same host. The reasons and the full policy are in [Trust and security](../TRUST.md).

*Checked against the code at master `13a673a`.*
