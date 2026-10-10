# Ask ws from Alfred

A one-line question from the Mac, answered by whatever model is fastest right now: an Alfred keyword that calls ws's OpenAI-compatible endpoint with your own key, through the router's `fast` rule. Any launcher or shell alias that can run a script works the same way; Alfred is the one described here.

## What the `fast` name does

`config/policies/default.yaml` has a rule that matches the model name `fast` and ranks its candidates by the decode speed the gateway has measured for each endpoint, scaled for how busy it is (see `rank: throughput` in [Configuration](../CONFIGURATION.md#policies)). Cerebras-hosted gpt-oss-120b, the local models and Haiku are the candidates; which one answers depends on what is fast at that moment, and the reply says which it was. Because it is a rule and not an alias, it can carry a rank; the API, Settings and the MCP server list it with the aliases all the same. Send `model: "fast"` on `/v1`, or mint a key whose default policy is `fast` and send no model at all.

## Set it up

1. **A key.** Open Settings in ws, create an API key with the default policy `fast` (the `chat` scope it has by default is all this needs), and save the key on one line in `~/.config/ws/key` (`chmod 600`). The key is shown once.
2. **The script.** Copy `scripts/alfred/ws-ask.sh` from the repo somewhere on your path, or point Alfred at it in the checkout. It needs `curl` and `jq` (`brew install jq`). Try it from a terminal first:

   ```sh
   WS_URL=http://ws.example.ts.net:8180 ws-ask.sh "what is the capital of Peru"
   ```

   `WS_URL` is where ws answers for you (the examples use a made-up tailnet name; use your box's tailnet address or your tunnel's hostname), without a trailing slash. The answer prints; `routed to <endpoint>` goes to stderr so Alfred can show just the answer.
3. **The workflow.** In Alfred, make a blank workflow and add:
   - a **Keyword** input (say `ws`, "with space", argument required);
   - a **Run Script** action, language `/bin/bash`, input as `{query}` (argv), with the script body:

     ```sh
     export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
     export WS_URL="http://ws.example.ts.net:8180"
     exec ~/bin/ws-ask.sh "$1" 2>/dev/null
     ```

   - an output: **Large Type** shows the answer on screen, **Copy to Clipboard** keeps it, **Post Notification** is enough for short answers. Chain two if you want both.

   Type `ws how long do I boil an egg` and the answer appears. For a different model or alias, set `WS_MODEL` in the script (`local`, `best`, an endpoint id); for a persona, `WS_SYSTEM`.

## Notes

- The call is yours: it is recorded under your account in the ledger and counts against your budget like a chat turn. A hosted candidate serves a member only with a key of their own or the owner's shared-key grant; local models always serve.
- The script sends the key as a header from a private temp file, never on the command line, so it does not show in the process list.
- `stream: false` keeps the script simple; the first token arrives no sooner either way for a one-shot answer.
- The same script, with `WS_MODEL=code`, is a quick way to ask the code route from a terminal.
