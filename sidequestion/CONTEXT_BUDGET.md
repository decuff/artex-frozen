# Bypass long conversations and context budget

Fixed on 2026-09-11. The original implementation treated the character count of the request JSON directly as tokens and inherited the main task's 32K output reserve, so when there was a lot of HTML, JS, and tool results, normal bypass questions were rejected prematurely.

## Open-source implementation review

- [Grok CLI bypass context](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/agent.ts#L739): extracts snippets from the most recent user and assistant text, with a character budget of about 2000 and at most 400 characters per item. It does not maintain a continuous bypass Q&A history on this path.
- [Grok CLI independent request](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/utils/side-question.ts): independent cancellation signal; output limit of 2048 tokens when the model supports it; no tools provided.
- [Grok CLI main session compaction](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/compaction.ts): estimates tokens, retains recent content, updates new content into the old summary, and handles cross-turn truncation.
- [OpenCode session compaction](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/compaction.ts): full request estimation, output/buffer reserve, recent content plus rolling summary, tool-free summary request; this implementation defaults to a recent budget of 8000 and a summary output limit of 4096 tokens.
- [OpenCode overflow recovery](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/runner/llm.ts): attempts overflow recovery only when assistant output has not yet started, and the recovered call no longer enters the same overflow recovery path.

ARTEX borrows the approach of an independent output budget, recent content plus rolling summary, and limited recovery. It keeps norma v0.3.6's structured messages and tool pairing, does not copy Grok's text excerpts, and does not write OpenCode's main session compaction events into the ARTEX main transcript.

## Request budget and execution

- Messages follow norma's per-content-block UTF-8 byte estimation and 4/3 margin; system prompt, tool schema, and message wrapping overhead are additionally counted. The estimate is not an exact model token count.
- Bypass output defaults to at most 8192 tokens and does not exceed the output limit already set in the main configuration. The service environment variable `ARTEX_BTW_MAX_OUTPUT_TOKENS` can set a limit of 256–32768; it does not modify the product default model or main task parameters.
- The input budget is the context window minus the output limit and safety margin; an unknown window uses the platform default of 200K. The safety margin is 5% of the window, with a minimum of 128 and a maximum of 8192 tokens.
- Successful Q&A loads at most 20 groups per batch in ascending ordinal order. At most 20 groups of original text are retained, with a token budget of at most 1/4 of the input budget and no more than 16K.
- Excess Q&A is updated into the rolling summary. The summary carries historical sources and context time; historical assistant answers are not equivalent to new tool evidence, and on conflict the latest main snapshot takes precedence.
- If the main context is still too long, only old messages in the summary copy are summarized, retaining at most 8K tokens of recent content; the cut point does not split tool calls from results. An oversized single group goes entirely into the summary.
- Summary input is chunked UTF-8-safely according to the actual remaining window, with an output limit of 2048 tokens; empty summaries, truncation, tool calls, or exceeding the summary budget are not written to cache. A single bypass makes at most 12 summary calls, subject to the same 120-second timeout; reaching the limit fails explicitly rather than looping indefinitely.
- If the model first returns a context overflow and has not yet output text or tool calls, retry at most once after further reduction; if the estimated size has not decreased, stop recovery immediately. Other model errors and partial streaming output do not trigger this recovery.
- All obtained usage, including usage from summaries, failed attempts, and cancellation, is accumulated into the same bypass request. When the provider does not return usage, only zero can still be recorded; estimates must not be disguised as actual usage.

## Persistence and UI

`side_question_sessions.memory` stores the old Q&A summary, the covered ordinals, and the main context summary cached by snapshot identity. `side_question_requests.context_info` stores the preparation stage, the actual number of replayed groups, summary usage, and budget estimates.

The summary is saved only when the original request is still running and the cleanup version matches; clearing also clears the cache, and late writes do not restore cleared data. A new snapshot does not reuse the old snapshot's summary. Summary fields are saved with the v3 task archive; when restoring old v3 records with missing fields, an empty object is filled in, and v1/v2 remain compatible.

POST first accepts and returns the request, and preparation and compaction run in the background without holding the admission lock or a database transaction. SSE/history shows the preparation, Q&A organization, copy compaction, and answer stages; a compaction failure is saved as the failed terminal state of that bypass request. The frontend retains the error and restores the draft of the failed question, without using a floating notification to obscure the input box; history polling no longer clears submission errors.

## Verification record

- 19/20/21/50-group replay, retention of old conclusions across 20 groups, and summary cache reuse after restart: automated tests pass.
- Very long Chinese responses and code context, chunked request budget, tool pairing, snapshot unchanged, new snapshot cache invalidation: automated pass.
- Summary failure/cancellation/truncation/overlong/tool return, clear race, call limit, only one overflow recovery and partial stream no retry: automated pass.
- Standalone PostgreSQL pagination, restart, v1/v2/v3 archiving, summary cache and budget metadata archive recovery, old v3 missing new fields, 20 parent sessions sharing four concurrent slots: pass.
- Go candidate service build, frontend TypeScript check, Biome check of modified components, and Next.js production build in a standalone directory: pass.
- Built-in browser using standalone UI fixtures, verified at 1280×720 and 390×844 the organizing phase, summary range hints, draft recovery after failure, no floating error notifications, no horizontal overflow, and no console errors. Temporary fixtures removed.
- Read-only replay of existing local Worker snapshots, new budget check passes; for example Worker #3's 293085-character snapshot is no longer falsely rejected by local character counting. This item made no external model calls.
- This time the real conversation test sending a private Worker snapshot to Grok was rejected by automated approval review, was not executed, and does not count as a pass.
- 2026-09-11 00:37 restarted the local backend as requested by the user, reusing the original database, data directory, and login configuration. Running file and candidate binary SHA-256 match; backend and frontend proxy `/api/health` both return normal.

Verification commands (using only a standalone test database):

```sh
go test -race ./sidequestion ./db ./server -run 'TestSide|TestCheckpoint|TestSnapshot|TestBuildRequest|TestService|TestMainSide|TestTaskArchive' -count=1
go build ./cmd/artex
npx tsc --noEmit
npm run build -- --webpack
```

The frontend production build uses a standalone copy to avoid overwriting the current preview's `.next`. The candidate service is located at `/private/tmp/artex-btw-budget-candidate`, copied to `/private/tmp/artex-btw-preview/artex` and started; the original binary backup is `artex.before-context-budget` in the same directory.
