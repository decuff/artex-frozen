# Changelog

All notable changes to this project are documented in this file, following the format of [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/).

## [Unreleased]

## [0.3.15] - 2026-10-07

### Interception

#### New Features

- **New built-in interception rule 'delete-type endpoint paths'**: Previously the built-in HTTP destructive rule only recognized the DELETE **method**, while most applications' delete endpoints can be triggered with GET/POST, so calls like `curl 'http://t/api/user/delete?id=1'` would actually delete the target data. A `deny` rule is now added covering `/delete /del /remove /unlink /erase /destroy` (the verb must be followed by a separator, so `/delivery`, `/details`, and `/delta` are not falsely intercepted). Existing instances will also receive it after upgrading, and it can be disabled or deleted in 'System → Command Interception'.
- **Token consumption of model fallback approval can be viewed separately**: Approval calls previously had no independent metering attribution, so how much was spent could only be guessed. Now it is metered separately, and a usage card is added below the 'Model Fallback Approval' toggle in 'System → Command Interception' — cumulative statistics for five items: call count, input, output, cache read and write, plus a mini bar chart of daily consumption over the last 30 days.

### Vulnerability Push

#### New Features

- **Vulnerability discovery supports pushing to IM**: A new 'System → Notification Push' page is added, which can push scan findings to six channels: **DingTalk, Feishu, WeCom, generic Webhook, Telegram, and Email**. Any number of instances can be configured for the same type, each with independent enable/disable, rate limiting, and filter rules.
- **Push timing has two modes: real-time and digest**: Real-time mode sends each hit as it occurs; digest mode merges them into a single message per global period (default 30 minutes), with the new count and severity distribution at the top. To do 'high severity real-time, the rest digest', just create two channels and configure them separately.
- **Filter rules support four dimensions**: minimum severity level, restricted task / asset scope, vulnerability type keyword inclusion and exclusion (exclusion takes priority), and whether to receive disposition status changes (off by default). Messages carry a 'View Details' button, whose target address is configured by the global 'Backlink Address'; if left empty, no button is included.
- **Delivery history and manual resend**: Lists each delivery's status, attempt count, failure reason, and owning channel, filterable by channel and status; failed items can be resent with one click (resending resets the retry count).
- **Channel credentials echoed as masked values**: Webhook addresses, signing secrets, Bot Tokens, SMTP passwords, etc. are only echoed as masked values with a trailing hint; submitting them back as-is means 'no change', and clearing the input box deletes that field.

#### Fixed Issues

> This section is the conclusion of a round of security auditing after the initial version of the feature was completed; each item was first reproduced, then fixed, then covered with regression tests.

- **Fixed masking being bypassed by 'changing the target address while keeping credentials' (severe)**: Config merging always retained the original value in the database for 'unmentioned keys', so by only changing the address and saying nothing about credentials, the server could be made to send the real credentials from the database to an arbitrary address, completely silently. It is now specified that once the target address changes, every credential field must be explicitly stated — either give a new value, or explicitly leave it empty to mean it is no longer needed.
- **Fixed markdown-based channels having zero escaping for untrusted content**: Vulnerability titles and summaries come from model output, and asset URLs come from scan results; a crafted title would render as a clickable external link in DingTalk / WeCom / Feishu, and image syntax would be fetched by the client at render time, leaking the reader's IP. Now single-lining plus markdown metacharacter escaping is applied uniformly, and the escaping happens at each channel's rendering exit rather than in the shared title function.
- **Fixed the SSRF surface of delivery addresses**: Previously loopback, intranet, and cloud metadata addresses could all be delivered to, and the first 200 bytes of the failure response body would go into `last_error` and be echoed in delivery history, forming a semi-blind read primitive. Now protection is set at the **dial stage**, covering DNS rebinding, and cross-host redirects are rejected; loopback and link-local addresses require explicitly setting `ARTEX_NOTIFY_ALLOW_LOCAL=1`, while RFC1918 private networks are still allowed (self-hosted intranet SMTP relays are very common).
- **Fixed push credentials leaking through error messages**: DingTalk's `access_token`, WeCom's `key`, and Telegram's bot token are all in the URL, and on delivery failure the full URL would flow into four places: `last_error`, delivery history, server logs, and frontend prompts. Now they are uniformly masked, keeping only `scheme://host` and the underlying reason.
- **Fixed silent loss caused by marking an entire batch as delivered after the digest message was truncated**: Truncated vulnerabilities were neither in the message nor in the failure list, yet delivery history showed success. Now packing is done by **whole item**, marking only those packed into this message, with the rest returned to the queue to continue in the next message without consuming retry counts, and the message header also states how many will continue in the next one.
- **Fixed a typo in the minimum severity threshold silently disabling the filter**: When `min_severity` was mistyped as something like `hgih`, the unknown severity ordinal was 0, degrading the check to always true, so users thought they were only receiving high severity while in fact all vulnerabilities were poured into the group. Now the value is validated on the write path and the allowed values are listed.
- **Fixed `rate_per_min = 0` (no rate limit) being unreachable**: The database write layer silently changed an explicit 0 to the default value, so the operator thought rate limiting was lifted while it was still in effect. The default is now filled at the API layer when the field is absent.
- **Fixed digest channels completely bypassing the token bucket**: `rate_per_min` previously had no effect on digest mode. Now the number of items claimed is constrained by both the current round's quota and an in-memory upper bound.
- **Fixed failure handling judging by the maximum attempt count of the whole batch, making old deliveries drag down new ones**: An old delivery that had already retried twice would drag brand-new deliveries in the same batch into failed together, so a new vulnerability was permanently lost without even one retry. Now it is decided per item.
- **Fixed deliveries in a digest batch whose snapshot could not be parsed being silently marked successful**: Such entries were skipped at the rendering stage but counted as successful along with the whole batch. Now they are explicitly judged as failed with a reason given.
- **Fix single-channel per-round delivery count possibly exceeding lease duration**: In multi-instance deployments, rows with expired leases get re-claimed, sent again, and have their attempt count incremented twice. Now the per-round limit is derived backwards from 'lease / single timeout', and an assertion pins down the relationship between these constants.
- **Fix Telegram truncation possibly cutting HTML entities**: Truncation only avoided half-cut tags, not entity fragments like `&amp`, and the parser may therefore reject the **entire** message. Now both are avoided.
- **Fix email channel treating temporary SMTP failures as permanent failures**: 4xx codes like greylisting `450` are temporary rejections that should be retried later, but were previously all judged fatal; servers with greylisting enabled would make every push fail after the first attempt. Now distinguished by the first digit of the response code: 4xx is retryable, 5xx is permanent failure.
- **Fix submitting nested structures actually writing mask literals into the database**: Object fields like `webhook.headers` can only be masked or submitted as a whole; stuffing the mask sentinel inside the object would be stored as a real value, causing authentication to silently fail. Now such submissions are explicitly rejected.

#### Design notes

- **The transaction writing the vulnerability does only one blind INSERT**: `notification_events` and the vulnerability are written within the same transaction, so committing guarantees both are atomically consistent. This INSERT deliberately does not read the channel table, does not run the user's filter rules, and is wrapped in a `SAVEPOINT`—otherwise a single misconfigured filter condition could abort the transaction, preventing a high-severity vulnerability from being stored.
- **Delivery uses lease claiming rather than long transactions**: After claiming with `FOR UPDATE SKIP LOCKED`, the row is set to `sending` and `next_attempt_at` is pushed into the future as a lease; no database lock is held during delivery. Rows left behind by a crashed process are re-claimed after the lease expires, self-healing without infinite retries.
- **Rate limiting does not consume the retry budget**: First compute this round's quota via the token bucket, then claim that many. Reversing the order would make deliveries blocked by rate limiting waste an attempt count, and after three budgets are exhausted by pure waiting, they fall into failure.

### Test infrastructure

#### Fixed issues

- **Fix permanent asset residue caused by cleanup order in the company ICP attribution test case**: `defer d.Close()` executes first when the function returns, and `t.Cleanup` runs after it, so all cleanup statements land on an already-closed connection, errors are discarded, and test assets and companies remain permanently in the database, also causing other test cases that assert on asset counts to fail inexplicably. Now closing the connection also goes through `t.Cleanup` and is registered first, while making cleanup failures visible.

### Traffic

#### New features

- **Traffic list supports content search, path / status code / length filtering, and column sorting**: The filter row supports filtering by **response content** (via full-text index), **path**, **status code** (exact value or buckets like `2xx`), and **response length range**; headers can be sorted by time, status code, and response length, with sort preferences saved locally. Two new indexes are added and take effect automatically after existing instances upgrade; the Agent tool's query contract is unchanged.
- **Traffic list adds 'Clear all'**: Deletes all traffic records at once (ignoring current filters), and cleans up historical host directories no longer recorded by the index; after clearing, it also performs a full compaction and reports how much space was actually freed. Traffic evidence bound to vulnerabilities lives in a separate evidence store and is unaffected.

#### Fixed issues

- **Fix disk space not being freed after deleting traffic**: The index database was created without `auto_vacuum` enabled, so the file never shrinks; the full-text index is also `contentless_delete`, so `DELETE` only writes tombstones without reclaiming the original postings—deleting traffic actually makes the index larger (measured: 6MB of body captured → 16MB index, still 16MB after deleting everything). Now new index databases enable `auto_vacuum=incremental`, and after each deletion, full-text index merging and incremental reclamation run in chunks in the background, gradually returning space to the system (same scenario drops to 104KB); reclamation releases the write lock between chunks, not blocking traffic recording.

> Upgrade note: `auto_vacuum` can only be set at database creation time, so **the index databases of existing instances remain in the old mode**, and incremental reclamation is a no-op on them (a hint line is printed at startup). On such databases, tombstone growth has already stopped after the upgrade, and the already-occupied size can be reclaimed once with 'Clear all'—that step converts the database to incremental reclamation mode, after which daily deletions take effect on their own.

### LLM

#### Fixed issues

- **Fix custom session header not taking effect on the 'one-off LLM call' path**: Goal decomposition (round 0) and cold-node compression are both one-off calls that don't attach a transcript store, so there is no session id in the context, and the gateway side therefore can't read that header—for endpoints like opencode zen that return 400 directly when the header is missing, this manifests as 'round 0 fails, but subsequent planner rounds are completely fine'. Now these two paths explicitly attach a session id stable per exploration, which also lets token usage be correctly attributed to the corresponding exploration (previously it couldn't be recorded at all).

### Tools

#### Fixed issues

- **Fix output truncation not taking effect for MCP and custom tools, letting large results flood the context**: MCP tool returns previously went entirely untruncated, while HTTP-type custom tools, due to passing an empty tool context, always used the hardcoded 6000 characters instead of the session-configured limit. Now both paths uniformly go through `actool.Capture`: truncating by the session's `MaxOutputChars` (default 30000), and when `ToolOutputDir` is configured, spilling the full output to disk with only the leading fragment and file pointer left in the message.
- **Upgrade norma to v0.4.3**: The harness layer uniformly executes `CaptureOnce`, so any tool (including ones added in the future) will no longer overflow the context due to missing truncation.

### Deployment

#### Fixed Issues

- **Fixed activity stream failing to connect under reverse-proxy deployments**: The SSE address was previously unconditionally assembled as `${hostname}:8787`; in deployments reverse-proxied to 443, the page loaded from `https://domain/` but SSE was pointed at port 8787, which was not exposed to the public internet, manifesting as the activity stream repeatedly reconnecting. Now differentiated by `NODE_ENV`: production defaults to same-origin, only `next dev` retains `:8787` to bypass Next.js buffering of SSE; the `NEXT_PUBLIC_SSE_BASE` override hook is retained. README has been updated with reverse-proxy deployment notes.

### Discovered

#### Fixed Issues

- **Fixed missing scrollbar when the asset list overflows in the 'By Asset' view**: The outer card only had `max-height` without a definite height, so the scroll viewport's percentage height could not be resolved; thus when there were many assets, the list either burst out of the card or was truncated and unscrollable. Now the height cap is applied directly to the asset tree's native scroll container, adapting to window height.

### Database

#### Fixed Issues

- **Set a cap on the connection pool**: `database/sql` by default does not limit the number of connections the application itself can open; when there are no idle connections in the pool, it unconditionally creates new ones, pushing all the way to PostgreSQL's `max_connections` (default 100) before being rejected. Now capped at 32; excess queries wait for an idle connection instead, manifesting as slowdown rather than errors under the same load; if `max_connections` has been lowered, it needs to be lowered accordingly.
- **Improved connection reuse rate**: The default value of `MaxIdleConns` is 2; connections beyond 2 concurrent ones are closed after use, and the next request must redo the TCP and PostgreSQL authentication handshake. Now the idle cap is raised to match the total cap, and a 30-minute connection lifetime and 5-minute idle eviction are added, allowing the pool to naturally replace long-lived bad connections.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.14] - 2026-09-24

### Task List

#### New Features

- **Added a 'Vulnerabilities' column to the task list**: Displays the counts of Critical / High / Medium / Low by severity tier, with non-zero tiers colored by severity, giving an at-a-glance view of each task's vulnerability scale and distribution.

#### Modified Features

- **Narrowed the widths of the 'Description / Goal' columns**: Overly long content is truncated with an ellipsis, and hovering shows the full text, reducing long text crowding the list's horizontal space.

### Exploration Graph and Planning Situation Overview

#### Modified Features

- **Streamlined the planning situation overview (graph_overview) and set count caps for each list**: Recent facts, ended intents, pending intents, cold-zone summaries, and confirmed vulnerability details are all changed to 'latest window + count fallback', with undisplayed items queryable on demand; significantly reducing the LLM context size per round and avoiding context bloat in long tasks (the cold zone of the related task overview is also rate-limited).

#### Fixed Issues

- **Eliminated the reverse duplicate edge between digest and finding in the exploration graph after collapsing cold nodes**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.13] - 2026-09-19

### Asset Interception

#### New Features

- **Added global asset interception rules (blacklist)**: Supports entering exact-match and fuzzy-match domains / IPs / URLs as well as CIDR ranges, providing CRUD and enable/disable for rules; the management page is located at 'System → Asset Interception'; by default, fuzzy interception of government (`.gov` / `.gov.cn`) and education (`.edu` / `.edu.cn`) websites is built in.
- **Asset interception integrated into the execution chain**: Before dispatching intents (`add_intent`) and inserting assets (`insert_assets`), the Agent first performs interception checks on the target assets—intents that hit interception are not dispatched, assets that hit interception are not inserted, and the asset information and interception reason are returned to the Agent.
- **Added task-level intercept / allow (whitelist) rules**: independent of global rules and effective only for this task, evaluated in the order 'intercept first, then allow' — a matching intercept rule blocks it; if no intercept matches but the task has allow rules configured and none of them match, it is 'not allowed to test'; if no allow rules are configured, the whitelist is not enabled. Can be entered when creating a task, or added, edited, deleted, enabled/disabled in the task details 'Overview'.
- **Task templates support preset categories and task-level intercept/allow rules**: templates can save the task category and a set of task-level rules, which are carried into the new task form when the template is applied.

### Operation Review

#### Fixed Issues

- **Tightened the model judge output protocol to reduce false passes caused by truncation**: lowered the verdict comment limit from 500 Chinese characters to 120 and enforced 'output only JSON, no preamble or code block', preventing the verdict from being truncated by `MaxTokens` and becoming unparseable, which would then pass under the model failure policy (fail-open).

### Task Archiving

#### Fixed Issues

- **Skip symlinks during archiving instead of failing the whole package**: the archive format only supports regular files and directories end-to-end; previously any symlink in the working directory caused the entire task archive to fail; now the symlink is skipped and logged, and the remaining files are archived normally (without following links or escaping the directory tree).

### Account and Compliance

#### New Features

- **Added a 'Usage Notice and Disclaimer' popup before login**: you must check to agree before you can log in.

### License and Dependencies

#### Changed Features

- **The project adopts the AGPL-3.0 open-source license**, and the README's license and disclaimer notes have been improved.
- **Upgraded norma to v0.4.1**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.12] - 2026-09-17

### Exploration Chain Broadcast Board

#### New Features

- **Added an 'Exploration Chain Broadcast Board' to the task details page** (#144): presents the task's exploration nodes (start/goal/intent/fact/vulnerability/hint/compression) in a timeline-stream view, supporting filtering by type, keyword search, ascending/descending order, pagination, and auto-refresh; when on the 'first page with newest first' it is the live position and refreshes each round, and when leaving that position it only accumulates an unread count without disturbing current reading, with a one-click 'N new broadcasts · back to latest' to return. Grouped by day, so even long tasks can be paged through while still recognizing 'which day this happened'.
- **Each broadcast board row shows the node id** (#145), making it easy to cross-reference the exploration chain graph and locate specific nodes.
- **Broadcast board node details expand to show upstream/downstream and anchored assets** (#147): expanding a broadcast shows the node's upstream (came from) / downstream (leads to) relationships, and hovering over a related entry pops up a card for the corresponding node (type/status/source/time/summary/payload snippet); it also lists the assets anchored to that node (type label + identifiable text). The relevant data is delivered along with the broadcast page, so expanding makes no extra requests.
- **Broadcast board search supports filtering by node id** (#150): in addition to 'content / source', the search box now matches node ids, so entering a pure number or the '#41' form shown in the UI precisely locates the corresponding node.

### Intent Management

#### New Features

- **Intent deletion supports two modes, 'soft delete / hard delete'** (#149): intents that are pending, running, or paused can all be deleted, and a reason is required; the deletion mode is chosen in the confirmation dialog.
  - **Soft delete (default)**: mark the intent as 'deleted' and record the reason in a separate field, keeping the intent node and all its outputs and lineage.
  - **Hard delete**: physically remove the intent, along with exclusive descendant nodes 'supported only by it' (cascading along the output / intent chain down to leaves), to avoid leaving orphaned data; the deleted intent's token accounting is archived and retained by original date; shared nodes (still referenced by other intents), targets, and task root facts are always retained, and the hard-delete dialog shows the estimated number of nodes affected by the cascade.

  Both modes notify the planner that 'the intent was deleted by the user + reason' and replan accordingly.

### Operation Review

#### New Features

- **Approval records can be filtered by status and decision source** (#139): filter approval records by approval status and decision source to quickly locate target records.

#### Fixed Issues

- **Pending approval requests load independently of history pagination** (#133): pending requests are no longer affected by history list pagination and remain fully visible while browsing history records.

### Agent

#### Changed Features

- **Worker role description changed to a general network security platform description** (#138).

#### Fixed Issues

- **Fixed cold node compaction never triggering**: connected a Compactor to the per-task planner; cold node compaction (cold-digest) previously never ran because it was not wired in, and is now restored.

### Chat

#### New Features

- **Chat supports @ references to multiple record types and scroll pagination** (#135): you can @ reference multiple types of records in chat, and reference candidates support scroll pagination loading.

#### Fixed Issues

- **Long message bubbles constrained within the conversation panel** (#137): overly long message bubbles no longer overflow the conversation area.
- **Fixed 'missing uploaded file' error when uploading files before a new conversation is created**: the Composer's file selection now snapshots the `FileList` into an array before clearing the input and then invokes the callback; previously, in draft state the conversation had to be created asynchronously first, and by the time execution resumed the `FileList` live-bound to the input had already been cleared, causing the upload to lack the file field and the backend to return 400.

### Traffic

#### Fixed Issues

- **Traffic recording proxy now listens only on 127.0.0.1 by default** (#129, #130): prevents the proxy from being exposed on other network interfaces under the default configuration.

### Web

#### Fixed Issues

- **Fixed the static export task list page losing the global header**.
- **Added the associated traffic mock for the demo vulnerability detail page, fixing the blank screen**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)
- [@dingpotian](https://github.com/dingpotian)

## [0.3.11] - 2026-09-15

### Operation Review

#### New Features

- **Approval records support source location** (#125): Clicking the 'source' of an approval record jumps back to the tool execution that triggered that approval — opens the original full session, automatically paginates to the target position, expands the command and result in place, and highlights it centered in the scroll area, keeping surrounding messages readable; auto-correction stops when the user scrolls manually. Across regular conversations, task Workers, the planner, and main Agent segmented sessions, precise location is achieved via persisted `tool_use_id` and task mapping (a new in-scope call ID index is added); when missing, duplicated, or ambiguously associated, an explicit prompt is shown rather than jumping to another execution. When the session is deleted or the task is archived/record missing, a clear prompt is given.
- **Approval records support pagination** (#115).

#### Modified Features

- **Streamlined model review input** (#124, #125): The model review (LLM judge after rules miss) input is narrowed to a versioned JSON of 'this one complete tool call + explicitly selected brief context + local working directory'. Context only takes real user messages (the current user message of chat/task main Agent); Workers no longer attach intent summaries and also clear context inherited from the parent Agent; the planner and auto-triggered sessions do not fabricate user messages. Task descriptions, goals, operation constraints, global exploration state, historical calls, or full Worker intent are no longer attached — these are still used per the original logic for Agent execution and independent session auditing, just not in operation review. All verdicts require outputting JSON `decision`/`comment`, and the explanation must include 'actual operation, consequence after success, matched rule'; instructions in parameters or context cannot change the review policy. A snapshot of the input actually sent to the model and its fingerprint are saved; old snapshots are retained and labeled with version, and current data is not used to fabricate old inputs.

#### Fixed Issues

- **Model verdicts wrapped in code blocks are no longer silently allowed** (#126): If the model-returned verdict JSON is wrapped in ``` code blocks, previously a parse failure would be silently allowed via the model failure policy; now the code block fences are stripped first before parsing, and only if parsing still fails is it handled per the configured failure policy.

### Agent

#### New Features

- **New experimental noa context compression** (norma upgraded to v0.4.0): An experimental feature that can be enabled in system settings, off by default. When enabled, noa (model-driven context compression) takes over context compression for the four Agent types — main Agent, planner, Worker, and chat — replacing the built-in compression; compressed originals are persisted and archived, centralized under `<workDir>/noa/<sessionID>/` (not scattered across task directories), with globally unique per-session-ID subdirectories. On integration failure it automatically falls back to built-in compression without interrupting real tasks; the switch is read once per run, and toggling only affects runs started afterward.

#### Fixed Issues

- **Exploration graph tools reject instead of nil-crashing when there is no task context**: Calling exploration graph related tools in a non-task context returns a clear error instead of crashing on a nil storage dereference.

### MCP

#### New Features

- **Support legacy SSE MCP services** (#117): Compatible with MCP servers that only provide the legacy SSE transport.

### Traffic

#### Fixed Issues

- **Traffic search matches recorded hosts with port awareness** (#114): Host matching in `traffic_search` now includes the port, preventing same-host records on different ports from interfering with each other.
- **A `traffic_search` description migration error no longer interrupts subsequent reporter migrations**: A single migration failure is isolated and does not affect subsequent migration execution.

### Web

#### Modified Features

- **Added dividers to task retest vulnerability options** (#122): Separators are added between retest vulnerability selection items for clearer visuals.

#### Fixed Issues

- **Fixed full-page crash on demo task detail page**: In mock mode, the task detail 'Sessions' tab fetches `GET /api/tasks/<id>/side-questions` (side-question history), but the mock handler had no such route, so the read fallback treated it as a collection because 'the path ends with s' and returned `[]`, causing `data.items` to be `undefined`, and the side hook's `merge()` iterating over it threw `TypeError: t is not iterable`; this exception occurred inside the `setItems` updater and was deferred by React to be rethrown during the render phase, so the caller's `catch` could not catch it, and the whole page was taken over by the error boundary showing 'This page couldn't load'. Now the mock handler explicitly returns an empty side-question history, and `sideAPI.history` also normalizes the return value (`items` is coerced to `[]` whenever it is not an array) as defense in depth.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.10] - 2026-09-13

### Network

#### New Features

- **Web search adds DeepSeek official source**: directly reuses the currently active LLM config. It differs in nature from the other three sources—DeepSeek has no directly callable search interface; search only exists inside its Anthropic-compatible interface (the `web_search_20250305` server-side tool), executed by the DeepSeek server. Therefore it **only supports the DeepSeek official endpoint + anthropic protocol** (OpenAI protocol endpoints will directly reject server-side tools), and each search **consumes an extra model call**, requests **do not go through the search egress proxy**, and are **not counted in traffic traces**; returned results **contain only titles and links** (no summaries; when body text is needed, WebFetch fetches it). The settings page explains the above limitations but **does not validate or block**; whether they are met is for the user to confirm, and the 'Test Search' button can be used to run a real verification.

### Agent

#### New features

- **worker gains cross-work lookback capability**: adds `search_all_worker_traces` (no need to know intent_id first; globally search for matching steps across the execution process of all works in this task by keyword) and `get_worker_trace` (after locking onto a work, list the step flow, search in place by keyword, and fetch full content by step_id), used to reuse observations seen in other works but not written into facts, avoiding duplicated effort.
- **`node_detail` exposed to worker**: together with the lookback tools above, after the worker gets the intent_id / node id, it can directly query the full details of that node.
- **Planning rounds triggered by `add_hint` are explicitly announced**: previously, newly added hints were only folded into the situation overview for the planner to discover on its own; now a single `add_hint` records a trigger for the planner—'Human added N strategic hints: …' (batched as one, not spamming one per hint)—and the planner is explicitly told 'this round was triggered by newly added hints' and directly sees the hint content.

#### Modified features

- **Global situation hints changed to a 'loosen up' tone**: encourage exploratory divergence and timely reporting of cross-intent clues, rather than premature convergence.
- **Streamlined worker boundary wording**: being blocked initially does not mean fully explored; focus on 'finish all bypass means within this intent before drawing conclusions'.
- `insert_assets` removes the `related` parameter for each asset: its only purpose was to decide whether to add this asset to the task scope, and the value is not persisted anyway (re-registering the same asset voids it, and the UI can't tell who was judged irrelevant); in practice it just made the model do one more judgment that isn't retained.
- **Test scope (`task_scope`) is no longer affected by the asset coverage toggle**: `insert_assets`'s automatic scope entry (`source='auto'`) executes regardless of whether coverage is enabled, and `add_task_scope` is always provided to the planner / in-task main Agent / goal-decomposition Agent. Scope is the task's authorization boundary and the filter basis for asset queries; the coverage toggle only decides whether to use it as the denominator for metrics, and should not decide whether to accumulate the scope itself. Previously, disabling coverage made both the `auto`/`agent` write paths fail simultaneously, leaving `task_scope` with only manually added rows from the UI. `list_untested_assets` is still hidden when coverage is disabled (it is itself a pure coverage perspective).

#### Fixed issues

- **Fixed cross-task asset mixing** (#59): `list_assets` previously hardcoded the task id as 0 and queried the entire shared asset library bare, and the model had no way to write scope filter conditions, so it treated assets from other tasks (especially IPs) as this task's targets, steering the test direction off course. Now it only returns assets within the test scope (`task_scope`) of [this task and directly related tasks], matched by **ownership** rather than literal value: if the scope contains a root domain, all its subdomains/services/endpoints can be found; if it contains a subnet, hosts and services within the subnet can be found; fetching an out-of-scope asset directly by id also fails. In non-task contexts (Auto/pentest) there is no scope to rely on, so it still falls back to the full library. Also fixes the ownership blind spot where IP-direct hosts (e.g. `http://1.2.3.4/api`) could not be matched by subnet scope. The UI's 'Test Assets' view (filtered by task producer) is unaffected.
- **Fixed worker cross-work lookback tools being mistakenly removed**: after `search_all_worker_traces` / `get_worker_trace` (and `node_detail`) were added to the worker's default toolset, an old migration that 'converged the worker tool surface' unbound them again at startup, so the worker actually couldn't get these tools. They have been removed from that unbind list, and for databases that already ran the old migration, they are re-bound to the worker once.
- **Fixed occasional persistence failure of `/btw` side questions**: before persisting the side_question checkpoint, strip the NUL (`\u0000`) escape that JSONB doesn't support, avoiding write errors for content containing that character.

### Triggers

#### Fixed issues

- **Merged trigger sessions deduplicate task descriptions/goals by task**: when multiple tasks are merged into one trigger, the same task's description/goal is no longer repeatedly concatenated into the message, avoiding long goals stacking up repeatedly and blowing up the context.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.9] - 2026-09-11

### Agent

#### New features

- The in-task main Agent supports **multiple sessions**: can create, switch, and independently reset context; each session is interactive.
- Supports persistent `/btw` **side questions**: ask follow-ups without interrupting the main thread; Q&A is persisted and still there after restart.
- `spawn_task` adds `source_task_ids`; the new task can **read-only inherit** the source task's assets and findings.

#### Modified features

- Disabled the worker's cross-engagement memory and converged its default tool surface: reading context and cross-work retrospectives are planning responsibilities; the worker only handles execution and write-back of a single intent.
- Streamlined the worker's default prompt; `get_worker_output` removes the `terminated` and `worker_name` fields; streamlined the descriptions of `insert_assets` / `list_assets`.

#### Fixed Issues

- Fixed the issue where work was interrupted early due to an **idle turn**: the model sometimes uses an entire round to output only thinking, giving neither body text nor a tool call; at this point the harness sees a natural end (`end_turn` with no `tool_use`) and directly finishes with `completed` + an empty summary — an intent that isn't done yet breaks off halfway, showing as 'the task ended normally but there is no text summary at all'. None of the five LLM retry layers can reach it: it isn't an error, the entry point for same-provider safe-window retry is 'stream failure', the circuit breaker directly judges success when `err == nil`, and intent rerun only recognizes `model_error`; the SDK's empty response retry also can't reach it, because that layer judges emptiness by 'whether any event was ever yielded', and the thinking delta itself is an event. Now work's Stop hook recognizes such a round and injects a continuation instruction, letting the model carry the thinking it has already produced and continue to the next step — deliberately not choosing 'resend as-is', because this kind of idle turn is usually determined by the prompt and the shape of the context, a stable behavior rather than random flakiness, and resending would only make the model think it through again.
- The count reuses the **empty response retry count** from the 'Retry and Backoff' section of the LLM page (both answer 'the model finished but produced no substantive content', only the criterion and the means differ), default 2, fill in `-1` to turn it off and fall back to the original 'idle turn means finish' behavior. It is a **total** cap per intent rather than a consecutive count: the harness itself already limits consecutive idle turns to pushing only once, and the quota is refreshed only when a real tool round has occurred, so this number guards against loops like 'tool → idle turn → push → tool → idle turn' exhausting the intent budget. When triggered, the log records `[work <worker> · #<intent>] idle turn… injecting continuation instruction (n/N)`. See section 1.1 of the upstream LLM retry design notes.
- Fixed the context budget and input layout of `/btw` in long conversations; degrade to generating a bypass request ID in non-secure contexts (non-HTTPS access).

### Traffic

#### New Features

- Vulnerabilities support **linking multiple traffic evidence items**, which can be sorted, annotated, and distinguished by role (request/response/corroboration).
- The report Agent will **automatically link** relevant traffic evidence before writing the report.
- Added an Agent **traffic binding switch**, and completed the handoff of evidence between Agents.

### Tasks

#### New Features

- Support **session-level vulnerability retesting**: retests run in an independent Agent session, and the list and details show the running status.

### Interception

#### New Features

- Approval records add **details and execution audit**: you can view the tool request context, the model/rule initial judgment, execution output, and parameter fingerprints.

### Assets

#### New Features

- Task test assets support **DSL search**.

### LLM

#### Fixed Issues

- Fixed the issue where the test connection did not send custom session headers, causing opencode zen to return 400.

### Network

#### New Features

- MCP's HTTP transport supports **skipping TLS certificate verification**, making it easier to connect to services with self-signed certificates.

### UI

#### New Features

- The session list is grouped by Agent, supporting expand/collapse and independent pinning; conversations support filtering by Agent.
- Compress the attack chain graph rendering (digest) nodes and collapse their members.

#### Fixed issues

- Fixed the white screen caused by login credentials not syncing.
- Changed the interception prompt text to explicitly indicate 'platform control', to avoid it being misread as defense on the target side.

### Deployment and updates

#### New features

- Support **one-click page update**: a 'Version and Update' card is added to the system configuration page, and the top bar shows a prompt when a new version is available. The flow is: download the release package → verify `SHA256SUMS` → smoke test → stage → exit so the supervisor script relaunches it to complete the swap, and the page refreshes automatically.
- Added supervisor startup scripts `start.sh` / `start.bat` as the official startup entry points (included in both the release package and the Docker image; `install.sh` is unchanged). They decide whether to relaunch based on the exit code and are responsible for forwarding SIGTERM to artex. The verification and swap logic are all in Go, keeping the scripts dead simple.
- Automatic fallback on failure: if verification or the smoke test fails, discard it and keep running the current version; if the new version fails to start 3 times in a row, roll back to the previous version. The settings page also offers a manual rollback (note that the database schema will not be rolled back).
- Updates only accept the GitHub domain and enforce HTTPS; the release source is not configurable; one-click update is disabled for development builds. GitHub query results are cached for 30 minutes to prevent the top bar prompt from exhausting the API quota.

#### Known limitations

- Under Docker, only the program is swapped, not the image: the toolchain will not be upgraded along with it, and recreating the container reverts to the version bundled in the image; use `docker compose pull artex` when needed.
- The `skills/` in the release package is not synced, so built-in skills added in the new version will not take effect automatically.
- Updating means restarting, which interrupts running tasks.

### Dependencies

#### Modified features

- Upgraded norma to v0.3.7.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.8] - 2026-09-09

### LLM

#### New features

- Added a 'Retry and Backoff' tab to the LLM page: the **count** and **interval** of all five retry layers are configurable. A single model call failure passes from inner to outer through five layers: connection retry (SDK; connection reset / timeout / 429 / 5xx before the stream starts), empty response retry (SDK; ends normally but has no content, openai format only), same-provider safe-window retry (replay of a broken stream before any output is delivered to the caller), polling circuit breaker (cooldown and skip once consecutive failures reach the threshold), and intent rerun (rerun the whole intent after the worker ends with model_error). The outer layer is only reached when the inner one is exhausted. Each layer has two knobs with unified semantics: leave blank = use the original default count and exponential backoff; fill in a count = use that count; fill in an interval = replace exponential backoff with a fixed interval; fill in -1 = disable that retry layer. The first three layers follow the endpoint and can override the global defaults field by field in each model configuration (those that only pin the interval still inherit the global count); the circuit breaker and intent rerun are process-level semantics with only one global copy. Changes take effect hot after saving, no restart needed. Leaving everything blank means current behavior, and an upgraded old database is byte-for-byte unchanged (new columns default to 0, and a missing settings key means all defaults). The connection test deliberately does not carry these parameters — it has a hard 30s timeout, and stacking user-configured retries on top would only turn a working endpoint into a timeout failure. See the upstream LLM retry design notes.
- Each LLM configuration supports a custom **session header** (`session_header_key`): when non-empty, every LLM request carries this HTTP header, with the value being the current running session id (chat sessions use `conv-<id>`, workers use `exp<x>-worker-i<intent>`, etc.), for gateways that do prompt caching / sticky routing based on a session-id header. The implementation reads the session id from the request context and injects it, requiring no changes to norma, and the same shared provider can also send different header values per session. Old databases get a migration, and the frontend LLM configuration dialog gets a new input field.

#### Fixed issues

- Fixed the `23502` error when saving an LLM configuration because the session header field was empty: the column is `NOT NULL DEFAULT ''`, and previously `NULLIF($n,'')` was mistakenly applied, writing an unfilled session header as NULL and triggering the not-null constraint; now the parameter is passed directly with empty-string semantics.

### Agent

#### New features

- Wall-clock timeout changed to **in-place wrap-up**: depends on norma v0.3.6; when `MaxDuration` is reached, it interrupts the running tool and continues on the live ctx for the wrap-up rounds (writing back recognized content + summary, with a final state of timeout), no longer relying on the worker/planner's respective `maxDur+90s` external hard ctx to kill a stuck run directly into `aborted_tools`. Chat goes through the same harness and automatically gets the same behavior; mainagent has no `MaxDuration` and is unaffected.
- Stall fallback: when the planner is woken by a heartbeat / no-change wake and the whole graph has no open or running intents, the opening line switches to a stall alert, explicitly stating that no worker is running and no direction is queued, and that this round must produce one or more mutually distinct new intents (must not produce 0 intents).

#### Modified features

- Worker prompt refactor: the raw JSON of intents / launch instructions / anchored assets moves into the system prompt, reassembled every round and never crushed by compaction, and continuation no longer depends on the first message of the transcript surviving; the launch user message is slimmed down to only the global situation overview (degradable, tolerates stale). The cost is per-intent data mixed into system and loss of cross-intent cache reuse, a deliberate trade-off for 'never lose intents'.
- Streamlined the default planner / worker body text and fixed several real-world issues: the planner now distinguishes `recent_done` by status (for blocked/exhausted, check the trace first before deciding, neither treating it as a dead end nor blindly rerunning), negative conclusions become 'observation / open question, not final' and evidence is checked before acceptance, when the goal is unmet and there are no open/running intents it must produce (hard floor), and depth takes priority over coverage; worker negative conclusions only write 'observation + tentative reading', the verdict belongs to the planner, and cross-intent clues go into the fact's summary for the planner rather than being chased by the worker itself. reseed appends the new default as a new version and switches to it, while user customizations / old versions are kept in history and can be rolled back.
- Converged the work agent's default toolset and asset write-back hints: the worker is only responsible for executing and writing back a single intent, while reading context / cross-work review is the planner's job, so `list_facts` / `node_detail` / `list_companies` and cross-work retrieval (`search_all_worker_traces` / `list_worker_traces` / `get_worker_trace`) are removed from its default tools, keeping only `list_findings` (dedupe check before reporting a vulnerability) + `add_finding` / `record_fact` + `insert_assets` / `list_assets`; the prompt drops stale field descriptions that conflict with the `insert_assets` schema (`type=tech`/`on_url`/`props`). Old databases get a one-time migration stripping the corresponding bindings; the same-named bindings of planner/main are untouched.

### Exploration graph

#### New features

- **Exploration graph cold-node compression (cold-digest)**: old and long-inactive intents / facts are folded into digest nodes shown in `graph_overview`, while the original nodes are kept permanently and can be fully restored by id (lossless storage, presentation-only compression, reversible folding). Cold/hot determination is by reverse reachability + any live branch means hot, with R=6 rounds of debounce and connected-component grouping; background compression (minor folds uncovered cold blocks, major recompresses from source to merge fragments) has liveness recheck and cooldown mutual exclusion, never touches the hot path and never overwrites revived nodes. The overview side provides `cold_digests` and an asset-indexed `cold_index`, with `expand_digest` / `expand_index` handling restoration. The overview of related / inherited tasks likewise reuses its own folded view, and `expand_digest` supports cross-task read-only restoration. `expand_digest` / `expand_index` are given only to the planner and main agent, not to the worker. All come with old-database migration.
- `graph_overview` outputs the full `finding_list`: findings are the task's highest-value product and a single task usually has few, so they are now brought out in full in the overview (unlike facts, which only give a recent window), letting the planner/worker see all confirmed vulnerabilities at a glance each round without calling `list_findings` again. Each entry is slimmed to `{id, summary, evidence?, from_intent?, assets?}`, where affected assets are given as readable content (url / domain / ip:port) rather than bare ids.

#### Modified features

- `graph_overview` no longer flattens `hosts`: the coverage block drops the host list (in large-scope tasks each round carried up to 500 host strings, of limited value to planning decisions), keeping only `host_count`, with specific hosts looked up on demand via `list_assets`. Added a top-level `done_intents_total` (total number of finished intents), parallel to `recent_done_intents` which is truncated to ≤15, so the planner knows when deduplicating whether anything was truncated.

### Tools

#### Modified features

- `add_company_scope` default binding changes from worker to planner: defining the enterprise asset scope is the job of planning / main control / Auto, and the worker only executes exploration. New databases seed the default binding as mainagent/planner/auto, and old databases get a one-time migration.
- The planner is bound to `list_assets` by default: it now has both `list_assets` (DSL full-database search) and `list_untested_assets` (untested within scope), backfilled once for old databases without overriding user unbinding.

### Tasks

#### New features

- The task list adds a 'Running Workers' column: counts the number of intent nodes with `state='running'` under that task, with exactly the same definition as the 'Running Workers' in the task detail page overview, showing 0 when there are no running workers.

### Network

#### New features

- Added a **global egress proxy** configuration: all target traffic can egress through a unified global proxy (http/https/socks5, supports `user:pass`). When traffic capture is enabled it serves as the upstream of the MITM recording proxy (traffic is still recorded and then egresses via the proxy; both the intercept and passthrough paths go through the upstream, not leaking the source IP); when capture is disabled it is injected directly into the agent's bash environment and WebFetch (`proxyEnv` gains `ALL_PROXY` support for socks5). The config is stored in the settings KV table (no migration needed), and the frontend system config page adds a 'Global Proxy' card, independent of the web search proxy and the LLM proxy.

### UI

#### Fixed issues

- Fixed the semantic errors of intent status labels: `exhausted` 'exhausted' → 'budget exhausted' (actually cut off mid-way upon reaching the step / time budget, with only partial results written back, not that the direction has been fully explored), `blocked` 'blocked' → 'execution error' (actually model / API / network failures with retries used up, the intent basically never truly explored, not a target / WAF block), and added the previously missing `stopped` 'stopped' (work manually stopped by the user).

### Dependencies

#### Modified features

- Upgrade norma to v0.3.4: MCP tool output now supports truncation and spilling to disk (see `651b961`); the follow-up v0.3.6 supports in-place wall-clock wrap-up.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.7] - 2026-08-31

### LLM

#### New features

- Added an 'output limit' to model config, with a choice of which request field name carries the limit: the limit caps how many tokens a single reply can generate and is sent with every request; 0 (default) = don't send the field, let the server default decide. It is distinct from 'context window' — the latter is the model's total capacity, used only locally to compute the compaction threshold and never appears in requests. The field-name toggle only matters for the `openai` (Chat Completions) format: empty (default) sends `max_tokens`, which the vast majority of compatible gateways only recognize; OpenAI's official reasoning models (o series / GPT-5) conversely only accept `max_completion_tokens` and will directly return `unsupported_parameter` when they receive `max_tokens`, so such endpoints need to be manually switched to the new field. The two formats each fix their field name (Anthropic uses `max_tokens`, Responses API uses `max_output_tokens`), so for non-openai formats the toggle is greyed out and cleared on save. Also wired up a path that had never been connected before: planner / worker / chat / main Agent / goal decomposition — none of the five ever set an output limit; openai-family never sent one, and Anthropic fell back to the SDK's 8192. Adding only a config item without wiring this path would mean the number never takes effect; now the value, like 'streaming output', is resolved per round from config, and takes effect on the next round after a failover switch. Behavior is completely unchanged after upgrading an old database (new column defaults to 0 and empty field name).
- When clicking 'Test' in the backend, the status code of each HTTP attempt and the gateway's raw response body are now written to the server log (response body truncated to 4K), making it easier to diagnose 401s, quota messages, empty frames, returned HTML pages, etc., instead of only seeing the UI-collapsed ok / err. This logging does not depend on the 'LLM records' toggle.

#### Changed features

- The task LLM config chain can now be modified in any state, no longer restricted to 'running / paused / config chain exhausted': after a task ends (done / failed / timeout), the main Agent conversation still uses this chain, and previously when a model on the chain had problems you could neither change it nor continue interacting. The terminal-state interception in both the backend HTTP and DB transaction layers has been removed, and the frontend modal now allows editing and saving for terminal tasks too. Saving a terminal task no longer reopens quota-blocked intents (those intents would be moved to open, with no worker to execute them and no longer meeting the conditions for 'rerun intent'); to keep running, still use rerun intent / add goal, which pull the task back into the running state.

### Agent

#### Changed features

- 'Send a message to a running Worker to adjust direction' now reuses the existing pause / resume + transcript resume mechanism, behaving the same as main Agent conversation: previously it used a self-built intervention persistence protocol that touched the scheduling barrier, the recovery flow, and a dozen-plus activity query filters. Now messages are injected as input for the next round, and the intent runs directly in a dedicated goroutine, not limited by the 3-slot worker pool and running as soon as it's sent; the frontend keeps the message box and restores the 'continue directly' button, with sending carried over SSE. No DB schema changes. Trade-off: messages are kept in memory only with no crash recovery, and sending a message may momentarily exceed concurrency by one worker for that task (a low-frequency scenario, acceptable).

### Tasks

#### Fixed issues

- Fixed an out-of-memory (OOM) crash when archiving large tasks: archiving now streams the snapshot to disk instead of reading the entire task into memory at once; also filled three recovery gaps on the cold-archive path — traffic from modern installs exists only in SQLite, and previously a crash between the PostgreSQL commit and the SQLite commit would leave an archived task's traffic stranded in hot storage with no way to recover it. Now a staging journal is written whether or not a historical directory exists, and a missing `journal.json` is treated as discardable.
- Fixed system lag when there are many tasks: loading conversations and loading the UI had noticeable delays. Queries for the task list, task context, and exploration records have been optimized together, and the frontend dashboard, conversation page, and task detail page now make fewer duplicate requests.

### Assets

#### Changed features

- Removed the 'max 256 rules' limit on enterprise asset scope: enterprises that enter scope IP by IP / domain by domain easily hit this cap, and once hit could only split into multiple enterprises, even though the scope itself isn't slower just because it has more entries. The limit has been removed in all three places — frontend, backend, and demo mock; a single rule is still limited to 1024 characters, and the 2 MiB request body limit is kept as a backstop (roughly forty to fifty thousand rules).

### Skills

#### Fixed issues

- Fixed the 'upload failed: zip: unsupported compression' error when uploading skill zip packages: Go's standard library only has built-in decompressors for Store / Deflate, so bzip2 and Zstandard packages written by compression software at non-default settings could never be unpacked. Both decompressors have now been added (pure Go implementations, no new external dependencies); for methods that truly can't be handled, such as Deflate64 / LZMA / XZ / PPMd, and for encrypted zip packages, a Chinese message is now reported before extraction, directly naming which file used which compression method and how to repackage it, instead of throwing the underlying English error at the user.
- Fixed the issue where filename validation inside skills rejected Chinese filenames as invalid: path validation was previously an ASCII whitelist of `[A-Za-z0-9-_./]`, so a zip package containing even one Chinese-named file (for example, a Markdown file with a Chinese directory and file name) would fail the whole upload with 'zip contains illegal path'. Changed to a Unicode blacklist: filenames in various languages and spaces are allowed, while control characters, invalid UTF-8, zero-width and bidirectional control characters (RLO filename spoofing), `\ % # ? * : " < > |`, and `..` / absolute paths / empty path segments are still rejected, with directory traversal protection unchanged. Skill names are similarly relaxed — ASCII is still limited to lowercase letters, digits, and hyphens (agentskills.io spec), while non-ASCII letters such as Chinese can be used directly as skill names, but spaces, dots, and path separators are not accepted.
- Fixed the issue where Chinese zip packages produced by Windows compression software had garbled filenames after extraction or were rejected entirely: such zips don't set the UTF-8 flag and write filenames in GBK; now they are decoded with a GBK fallback before path validation. Quoted Chinese values in the `name` frontmatter field are now read correctly.

### UI

#### New features

- The findings page adds a flat 'All findings' view and makes it the default, switchable via header tabs with the original 'grouped by task' view: the grouped view requires expanding each task one by one to see vulnerabilities, which is a detour when you just want to scan a list across tasks. The flat view is one large cross-task table (10/20/50/100 per page) with behavior identical to the grouped view — checkbox export, inline expansion to see evidence and detailed reports, inline editing of name/category/severity, changing disposition status, drill-down, delete. Both views share the stat cards and filter bar, switching doesn't lose filters, and the current view and filter selections are both remembered locally; polling only hits the current view, and inline changes sync to both caches so switching over won't show stale data.
- The findings page adds a 'by asset' view: on the left an asset tree (enterprise → root domain / IP → subdomain → service → endpoint; the enterprise level only appears when an asset actually has an owner), on the right the findings under the entire subtree of the selected node, sharing the same table and the same set of filters as the other two views. The tree only includes assets that have findings, ancestor chains are filled in on demand (when findings are attached only to the deepest endpoint, the upper levels can still be assembled), and the counts on nodes are subtree aggregates deduplicated by finding; findings whose asset has been deleted or that were never associated with an asset go into an 'unassociated assets' bucket. Unlike the other two views, the asset view doesn't poll — it queries only when entering the view, changing filters, adding/editing/deleting findings within the page, or clicking the refresh button on the tree; the left tree is a navigation structure, so there's no need to recompute it every 5 seconds. Each level of the tree shows only the increment relative to the level above (subdomains drop the root domain suffix, services show `https :443`, endpoints show only the path), with full values in tooltips and breadcrumbs; when there are too many nodes, the endpoint/service levels are dropped entirely for that level with a hint, and counts still roll up to the upper levels.

#### Fixed issues

- Fix the issue in mobile session details where session records are squeezed and can't be fully seen: collapse the session list on narrow screens to give the width to the records themselves.

### Dependencies

#### Changed features

- Upgrade norma v0.3.2 → v0.3.3: add `Config.MaxTokensField`, allowing the output limit of OpenAI Chat Completions to be sent as `max_completion_tokens` instead (reasoning models only recognize this key). The two keys are mutually exclusive, only one is sent, and the default remains `max_tokens`.
- Upgrade `golang.org/x/mod` v0.37.0 → v0.40.0, fixing two dependency security alerts.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)
- [@begininvoke](https://github.com/begininvoke)

## [0.3.6] - 2026-08-27

### LLM

#### Fixed issues

- Fix the issue where the 'non-streaming' toggle reverts to 'streaming' after saving and reopening (#69): the DTO of the config list endpoint was missing the `streaming` field, so the response never returned it, and the frontend read `undefined` and always fell back to the default streaming; the actual write and DB storage were fine, it just couldn't be read back. Add the field back to the DTO (without `omitempty`, `false` must also appear in the response).
- Change the test connection to verify the model actually replies, and test according to the config's real send/receive mode (#65): previously it only checked whether HTTP succeeded, so a request that went through but got zero reply from the model (thinking burned the budget / body swallowed by safety policy / compatibility layer dropped `content`) was also reported as 'connection successful', which was inconsistent with the 'no reply' behavior in sessions; and the test always used streaming, so a non-streaming config was actually testing a different channel. Now an empty reply is directly judged as failure, on success the model reply is shown in the prompt, and the current streaming toggle is brought into the test, keeping 'test passed' consistent with 'session runs fine'.

### Agent

#### Changed features

- `list_facts` changed to pagination + keyword filtering, to avoid a single call blowing up the context when there are many facts (#74): by default returns the latest 20, supports `limit` (max 100) / `before` cursor / `q` summary keyword, returns `{facts, total, has_more, next_before}`; an overly long single summary is truncated by character count (the full text still goes through `node_detail`). The worker / planner prompts are updated to pagination semantics. Old databases get the new parameter schema flushed into the tool catalog table via a one-time migration (`SeedTool` insert-only on first insert, otherwise the tool management page shows 'no parameters').

### Tools

#### New features

- The 'Tool Execution' page adds tool call count statistics (#72): the 'Statistics' button in the toolbar opens a dialog showing call count, percentage, and failure count per tool (in descending order of count); it reuses the list's task / keyword filters, counts the entire result set rather than the current page, and only fetches when the dialog is opened.

### Dependencies

#### Changed features

- Upgrade norma v0.3.1 → v0.3.2: fix reasoning/refusal silently dropped in OpenAI-family responses (Responses adds `reasoning_text`), dedupe the three reasoning field names, bounded retry for empty responses (zero content blocks), and gateway 400 caused by compression boundaries cutting tool pairing (also fixes existing orphans already baked into transcripts).

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.5] - 2026-08-25

### LLM

#### New features

- Each LLM config supports streaming/non-streaming toggle (streaming by default): on uses streaming SSE; off uses true non-streaming (`stream:false`, returns complete JSON at once), which can bypass some gateways' poor SSE implementations (empty frames, dropped thinking field frames), at the cost of losing real-time progress and real-time token counting during runs. worker / planner / mainagent / chat / goals all dynamically take the value from the currently active config; the three layers of Provider wrappers in `llmpool` / `llmrec` / task runtime are all compatible with non-streaming; `llm_profiles` adds a `streaming` column and `ALTER`s old databases (default `true`, old configs unaffected).
- Support LLM configs in OpenAI Responses API format: each config adds a third format `openai-responses` (hits `POST /v1/responses`), alongside Chat Completions / Anthropic; `BaseURL` normalization, default model `gpt-5`; the `format` constraint of `llm_profiles` adds `openai-responses` with an idempotent migration for old databases; the frontend format dropdown adds 'OpenAI (Responses API)'. Dependency upgrade norma v0.3.1 (includes `reasoning_content` echo fix).
- Record raw LLM request/response HTTP text: capture the real wire body at the HTTP transport layer, preserving tool schemas, `tool_use` blocks, and raw SSE frames that the normalized view can't see; multiple attempts of norma internal retries are kept one by one; `llm_records` adds two columns `raw_request` / `raw_response` and `ALTER`s old databases; the recording page detail panel adds a 'raw' view toggle and request/response copy buttons (with `execCommand` fallback for non-secure contexts).

### Chat

#### Changed features

- Session list multi-select changed to a 'multi-select' mode toggle: the default list no longer has a persistent checkbox on each row (cleaner look), the header changes to 'N total + Multi-select' button; clicking 'Multi-select' enters selection mode (checkboxes, select all, batch delete), 'Done' exits and clears the selection, batch delete automatically exits after all succeed (stays in selection mode on failure for easy retry); single rename / pin / delete still go through each row's ⋯ menu.

### UI

#### Modified Features

- Enhanced task management, session operations, and traffic viewing experience (#57): task list column sort preferences are now persistently remembered, inline renaming is triggered directly via an icon (no longer through a menu), and drawer (sheet) interactions and traffic viewing details are optimized.

#### Fixed Issues

- Worker asset tags now only display domain and IP, and correctly handle the empty-tag scenario.

### Agent

#### New Features

- Added 'quantitative acceptance verification' when the planner judges goals: when a goal contains quantifiable acceptance conditions (asset test coverage reaches X%, obtain N flags, gain a certain permission), before `prove_goal` it must verify the measured values from `graph_overview` (`coverage.pct`, findings count, etc.); if the measured values fall short, `prove_goal` is forbidden and instead intents should be dispatched to close the gap, and it must not be marked met early on the grounds of 'the main part is done / largely achieved'. Fixed the issue where a goal requiring 100% coverage but measured at only 40% was judged complete.

#### Modified Features

- Rewrote the criteria for '0 intents this round' in the planner prompt: the original text described 0 intents as 'the most common and most important principle', which caused the planner to stop too early when the goal was unmet and untested surfaces remained in scope. It now states that 0 intents should only be produced in two specific cases—① all directions thought of are already covered by open/running/recent_done intents; ② the next step depends on the output of currently running work, and that output has not yet appeared (it should wait for it to finish and plan again on the next wakeup after the graph updates). A reverse constraint is also added: when there genuinely are new directions not yet covered and not dependent on running work, or the goal is unmet and untested surfaces remain, do not stop just because '0 intents is common'.
- Strengthened the 'evidence threshold for negative conclusions' in the worker prompt: for negative conclusions such as 'not injectable / port closed / no login entry' that might cause the planner to abandon an entire direction, the worker is required to exhaust all reasonable means within that intent (change encoding/parameters/path/method) before concluding; if the means are not exhausted or the evidence is weak, always mark `confidence=inferred`, to avoid a hasty `observed` negation welding the whole route shut (an erroneous negation early in a task especially derails the direction and is hard to self-heal).
- The above planner / worker default prompt changes are appended as new versions and switched to the current version via one-time migrations (`reseedPlannerPrompt` / `reseedWorkerPrompt`, each guarded by a settings flag); old versions are kept in version history, and users who customized prompts can restore them from the version records.

### Operations

#### New Features

- Added `reset-password.sh` to reset the admin (username fixed as `ARTEX`) password: supports both local / docker deployments, connection info can be explicitly specified or automatically read from `--dsn`/`$ARTEX_PG_DSN`/`config.json`; it uses `pgcrypto` in the database to generate a bcrypt hash compatible with backend login and writes it back to `settings.auth.password_hash`, with no service restart needed after reset. The password is passed via environment variable, does not enter the process argv, and is escaped to prevent injection.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

## [0.3.4] - 2026-08-24

### UI

#### New Features

- After checking multiple tasks in the task list, their category can be changed in bulk; the target category supports 'Uncategorized' to move tasks out of the current category; the whole batch is written in a single transaction, and the only failures will be tasks that were deleted after being checked.

#### Modified Features

- The 'Task Category' for new tasks changed from a dropdown to a searchable input: typing filters existing categories, and a name not in the database is created and selected immediately on Enter (or by clicking 'Create' in the dropdown), with the selected item shown as a removable tag; still limited to a single category.

### Conversation

#### Fixed Issues

- Fixed the issue where, when a session had an LLM config selected but there was no globally active config, sending a message was blocked by 'LLM not configured, cannot converse'. The pre-send check originally only looked at the globally active config, while actual execution prioritized the session-selected config, so the two resolution logics were inconsistent; they are now unified into the same set (session/Agent binding takes priority, global as fallback).
- The prompt when conversation is unavailable is now broken down by state: when there is no config at all, it prompts 'add a config'; when there is a config but none is active, it prompts 'activate one or specify a config for this session', no longer always showing 'not configured', making it easier to diagnose.

### Agent

#### New Features

- For tasks whose goals are all achieved, intents dispatched by the main Agent via `add_intent` can be directly claimed and executed by the Worker and stop once done: at this point the Planner no longer runs (a task with no open goals does not enter planning), avoiding it re-judging goal achievement and canceling the just-dispatched intents; after the frontier is drained, the task returns to completed. Before dispatching an intent, the main Agent asks the user, based on the intent content, whether to register it as a formal goal; if registered, the task resumes regular autonomous planning.

#### Modified Features

- When `step_ids` in `get_worker_trace` / `get_task_worker_trace` exceeds the per-call limit (5), it no longer errors out directly; instead it returns the full content of the first 5 steps, and reports via `returned_step_ids` / `omitted_step_ids` / `notice` in the result which steps were taken this time and which were not, with the hint 'if that is enough, no need to fetch more'. Duplicate and invalid ids passed in are first deduplicated and removed before counting.

#### Fixed issues

- The task list is now sorted by task id descending (most recently created first), replacing the previous sort by creation time. Multiple tasks created at the same moment share the same timestamp and sort unstably; combined with the 10-second polling and random iteration over the in-memory map, they would frequently swap positions in the list.

### Assets

#### Fixed issues

- Fixed enterprise ownership recalculation being broken by a single row of dirty data: when `assets.ip` stores a hostname (written by the Agent or the asset API), `a.ip::inet` throws `22P02`, causing adding/modifying enterprise asset scopes and deleting an enterprise to all fail and roll back. Switched to the safe conversion `try_inet()`, so invalid values are skipped instead of aborting the whole statement.
- Unparseable `ip` values are no longer silently skipped: after saving enterprise scopes, it explicitly reports how many assets have an `ip` that is not a valid IP (including the specific id and value), warning that these assets will not be assigned by IP/CIDR rules; the same information is also written to the server log, covering paths with no frontend response (deleting an enterprise, scopesentry sync, Agent writes).
- IP/CIDR matching for task test scopes now also uses `try_inet()`, replacing the previous regex guard that only checked the character set (hostnames made entirely of hex letters like `abc.def` previously still slipped through and triggered the same error).

#### Changed features

- The `ip` field of `ip`, `service`, and `endpoint` assets no longer accepts hostnames: `insert_assets` and the asset API return an error with `index` for each item, and directly give the fix (use `type=subdomain` with `domain`, or resolve the A/AAAA records first), so the Agent can correct it and re-insert. Other assets in the same batch are unaffected and are stored as usual.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.3] - 2026-08-23

### Worker

#### New features

- A running Worker supports pausing, resuming, and cancelling individual items; pausing preserves the intent, facts, and vulnerabilities, while cancelling transactionally cleans up the current intent and its direct products after execution exits.
- Added a `paused` intent state, an execution fence, and named termination reasons, preventing late blackboard writes after pause, cancel, or task deletion.
- Added an optional task concurrency limit; new creation, resume, vulnerability deep-dive, and queue refill all go through a persistent FIFO admission path.

#### Changed features

- The default wall-clock duration of a single Worker run is changed from 600 seconds to 1200 seconds; a timed-out task that is revived restarts its timer on the next real execution.
- The real execution status of the Worker, Planner, and main Agent now uniformly drives the task status badge, fixing the issue where the Worker still showed 'idle' while running.
- The Worker session retains per-item control and the current session Token summary; the model name is moved to display to the right of the current session title.

#### Removed features

- Removed the Worker multi-select, select-all, and batch pause/resume UI, along with the Worker batch control API and Mock contract.
- Removed the Token badge and Tooltip from Worker list rows; the underlying full Token ledger and task aggregation API are retained.

### LLM

#### New features

- Tasks support an ordered LLM configuration chain; when a provider's insufficient quota is clearly identified, it automatically switches to the next configuration, and persists the current configuration, exhaustion state, and error summary.
- Running and paused tasks support editing the full configuration chain, adjusting the order, and manually switching the current configuration, taking effect from the next LLM call.
- Automatic switching, manual switching, and full-chain exhaustion are written to structured system activity, with deduplicable reminders surfaced through the task activity stream.
- Added a task role model resolution interface that uniformly resolves the configuration and model used by the Main Agent, Planner, and Worker on their next call.
- Added an optional global LLM Pool, supporting configuration of call order, fallback when a specified model fails, health status, cooldown recovery, and manual reset.
- Added an always-on LLM usage ledger that aggregates input, output, and cached tokens by task, session, model, and configuration.

#### Modified Features

- Goal decomposition, Planner, Worker, and the main Agent share a task-level LLM runtime (resolution priority is listed below).
- Failover only responds to explicit quota, balance, or billing errors; ordinary rate limiting, authentication, network, server-side, and context errors will not trigger incorrect switching.
- The LLM configuration page is changed to a configuration card list with right-side drawer editing, and provides Pool polling order, priority, exclusions, health status, and recovery operations.
- Configuration chain exhaustion information supports line wrapping on narrow screens; the current model is changed from an icon in the session list to a text label next to the current session title with a full Tooltip.
- The order in which each role within a task resolves the LLM is adjusted to 'Agent binding → task configuration chain → global': roles with an explicitly bound model always run on that model, only unbound roles fall to the task chain, and when the task chain is empty they fall to the global.
- Leaving the outbound proxy blank means direct connection, no longer falling back to the `HTTP_PROXY`/`HTTPS_PROXY` environment variables (an explicit `ARTEX_LLM_PROXY` is unaffected); proxy input supports `socks5://user:pass@host:port` with account and password.

#### Fixed Issues

- Transient streaming failures before submission (before any output has been produced) are safely retried with backoff on the same provider, significantly reducing cases of 'interrupted after running only one or two tools, no summary, final state `model_error`'; quota exhaustion, overly long context, and deterministic 4xx errors are not retried and are still handled by failover and compression recovery respectively.

#### Removed Features

- Removed the LLM icon-style indicator from the session list to avoid confusion with the Worker status icon.

### UI

#### New Features

- Tasks support creation, renaming, deletion, and filtering of global categories; new tasks can directly select a category.
- Added global task template CRUD and a right-side management drawer; new tasks can load preset descriptions and goals, and current content can also be saved as a template.
- New tasks support associating multiple source tasks and multiple enterprise asset scopes; enterprise scopes automatically recognize domains, URLs, IPs, CIDRs, ICPs, and enterprise keywords through a single multi-line text box.
- Task test assets support real-time addition and deletion, recording sources such as manual, enterprise, inherited, or Agent-discovered; the current test asset and source summary are displayed next to the Worker session title.
- The discovery page groups by task and provides independent pagination within groups, supporting filling in descriptions for vulnerabilities and creating high-priority deep-dive intents.
- Conversations support renaming, pinning, unpinning, and deletion; the task list supports batch pause and resume for the current page.
- Traffic details and tool execution details are changed to right-side drawers; HTTP messages now include `Host` and highlight the request line, status code, Header, JSON, and markup language body.
- The task list action column adds 'Details' and 'Pause/Resume' buttons; the session send key binding can be configured in system settings (`Enter` / `Cmd+Enter`, etc.), and a proxy input box is added to the Web search area.
- The badge to the right of the session title is changed to display the LLM configuration name, with the model ID moved to the hover Tooltip; tasks can be given an optional name, and the task list adds a name column and search (falls back to description when empty).
- The Skills page now shows usage counts, last-used times, and missing-dependency statistics, making it easier to identify skills that are not taking effect or not installed.

#### Changed features

- Task titles are now focusable detail links; task assets, enterprise assets, discovery task groups, and vulnerabilities within groups use real server-side pagination, stable sorting, and exact totals.
- Adding an enterprise now uses a right-side drawer; scope entry is unified as multi-line text with real-time recognition, validation, and type preview.
- The main Agent input box supports auto-growth, `Enter` to send, `Shift+Enter` for a new line, and avoids accidental sends during Chinese IME composition.
- Agent previews, task reports, and related details now uniformly use shared Markdown rendering; delete confirmations, long errors, and mobile drawer widths are uniformly fixed.
- The task template selector displays and searches by template name and submits by template ID; the system's original font size is restored, and the app version is uniformly displayed as `0.3.3`.
- Task totals and current-session Tokens now highlight only the input, cache-read, and output values; the send button uses a simpler up-arrow icon.
- The test scope in the task overview shows enterprise names instead of IDs.

#### Fixed issues

- Plain text containing dots is no longer misidentified as an ICP filing number.
- Fixed an issue where variables in the variable directory collided with global runtime variables (such as `{{.Now}}`), causing duplicate keys to be rendered in the Agent editor's variable list.

#### Removed features

- Removed the separate 'Enter' button on task cards; clicking the task title now uniformly opens the details.
- Removed the 'New Task' button on the dashboard, the Logo URL input when adding an enterprise, and the vulnerability count Badge at the top of the discovery page and in task groups.
- Reverted the global 10% font-size increase and removed the Worker selector, model icon, and row-level Token statistics from the session list.

### Agent

#### New features

- New tasks can directly link to multiple existing tasks, inheriting in real time and read-only the goals, facts, vulnerabilities, completed intents, asset scope, and blackboard context of the direct source tasks.
- The blackboard read tool supports on-demand queries of source task nodes, facts, vulnerabilities, and execution traces; inherited nodes carry source markers, and all write tools refuse to modify them.
- The enterprise scope tool supports domains, URLs, IPs, CIDRs, ICP, and enterprise keywords; keywords serve only as Agent scope hints and do not participate in automatic asset attribution.
- The vulnerability deep-dive action creates a high-priority manual Worker intent in the original task with an asset anchor and a `derived_from` edge.
- The overview adds a 'Goal Management' card that supports manual viewing, adding, modifying, and deleting goals; adding or modifying notifies the Planner and revives the task, while deletion hard-deletes the goal node (cascading deletion of edges and anchors) without reviving.
- The main Agent now gets the `steer_work` tool by default, allowing real-time corrective instructions to be injected into running Workers without interrupting them or losing progress (intent ownership by the current task is still validated).

#### Changed features

- Source task intents do not enter the new task's frontier; the new task continues to use its own independent exploration, execution queue, working directory, and conversation history.
- Deleting a running intent no longer destroys data: instead it stops at `stopped`, requires a deletion reason, attaches the reason as a fact to that intent and writes it into the payload, and the planner triggers perception via `cancelled` (preserving the intent content and reason).
- The main Agent conversation is now driven by a server-side activity stream, and failed sends can recover the input; opening a session auto-scrolls to the bottom and keeps the last reply visible after lazy-loading details.
- Pausing a task terminates the current Main Agent, Planner, and Worker calls, but does not prevent the user from actively starting a new main Agent orchestration session during the pause.
- Cancellation, closure, and streaming interruption uniformly preserve the real termination reason, generated content, run count, elapsed time, Token, and unreturned tool calls.

#### Removed Features

- Removed the main Agent client-side optimistic message echo to avoid duplicate messages and cross-session streaming under pauses, failures, or concurrent activity.

### Constraints

#### New Features

- The goal decomposition stage first extracts operational constraints (allow/deny) before decomposing goals, and the main Agent can supplement them at runtime; constraints are injected into the Planner and Worker system prompts at the highest priority, and after debiasing, diversity and broad exploration explicitly defer to the constraints.
- The overview adds a 'Constraint Management' card and add/delete/edit interfaces; the constraint injection scope can be toggled separately for Planner / Worker (both on by default, read each round, effective immediately). The new table `task_constraints` uses `CREATE TABLE IF NOT EXISTS`, and old databases are automatically created.

### Interception

#### New Features

- Command interception adds model fallback approval in addition to regex/string rules: when no rule matches, the model makes a semantic `ALLOW`/`ASK`/`DENY` judgment, and the fallback action on failure and the approval timeout action are configurable; the interception page is split into two tabs, 'Interception Rules / Model Configuration', and model judgment results are prefixed with `[Model]` and a reason.

#### Fixed Issues

- Hardened the judge output parsing to avoid a correct `DENY` judgment being mistakenly treated as an allow.

### Traffic

#### Modified Features

- Traffic recording now stores the entire exchange in SQLite (large bodies overflow to a hash-deduplicated blob bucket), builds a trigram full-text index on text bodies, and supports arbitrary substring and Chinese search; deletion degrades to a single SQL transaction, dropping from hours to milliseconds without stopping recording during the process. Added large-body streaming download and the `traffic_search` full-text parameter; old file-tree data requires no migration and remains readable, searchable, and deletable.

### Tasks and Assets

#### New Features

- Task creation adds an 'Asset Coverage Feature' toggle (on by default): when off, coverage is not calculated or displayed, the situation map shows only assets, `task_scope` is not automatically accumulated, and related tools are removed from the Planner / main Agent; enterprise association is unaffected by this toggle.
- `insert_assets` adds a `related` flag to each asset (default `true`): it takes effect only when coverage is enabled, and `false` only enters the shared asset library and is not counted in this task's coverage (e.g., incidentally discovered collateral sites or unrelated assets).
- Enterprise scope and task test assets uniformly support domain, URL, IP, CIDR, ICP, and keyword text recognition; domains/IPs can create or reuse global assets, while CIDR/ICP/keywords are retained as task scope context.

### Build

- `build.sh --release` supports building Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 in one go, and generates a zip release package with `skills/`, configuration examples, and README.
- Use the Go linker to strip debug information and generate a zip release package; UPX is now explicitly optional to avoid segfaults when its self-extracting ELF starts in some Linux environments.
- The Release Workflow runs a startup smoke test on the Linux amd64 binary and provides `SHA256SUMS` with the release package.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.2] - 2026-08-20

### Added

- New tasks can be linked to multiple existing tasks, inheriting in real time and read-only the facts, vulnerabilities, completed intents, asset scope, and blackboard context of the direct source tasks; new tasks still use an independent execution queue, working directory, and session history.
- Tasks support an ordered LLM configuration chain. When a provider quota shortage is explicitly detected, it automatically switches to the next configuration and persists the current configuration, exhaustion state, and structured audit activities.
- Running and paused tasks support editing the LLM configuration chain, adjusting the order, and manually switching the current configuration; automatic switching, manual switching, and full-chain exhaustion are indicated on the task detail page.
- A running Worker supports pause, resume, and cancel. Pause retains blackboard data; cancel transactionally cleans up the intent and the facts, vulnerabilities, and execution records it directly produced after the Worker stops writing.
- Deleting a task can optionally clean up associated assets, traffic, vulnerabilities, and task output files, and adds a deletion barrier, concurrency protection, and auditable deletion statistics.
- Add an optional task concurrency limit; new tasks that reach the limit are queued in FIFO order and automatically started when a running slot is released.
- Add task asset pagination, company asset pagination, task category statistics, and a paginated intent Mock contract.
- Add a `build.sh` single-binary build script supporting static frontend export, resource embedding, cross-platform targets, and build version injection.

### Changed

- Task-level explicit LLM configuration chains coexist with the existing global LLM Pool; explicit chains continue to use strict quota failover semantics, and when there is no explicit chain, the Agent binding and global configuration rules apply.
- The main Agent input box supports auto-growing multi-line input, `Enter` to send, `Shift+Enter` for a new line, and avoids accidental sending during Chinese IME composition.
- Planner, Worker, and the main Agent share the task-level LLM runtime; the task chain uses the smallest context window among candidate models as the safe compression threshold.
- The task detail page displays running, idle, and paused states based on the real LLM call status; facts, vulnerabilities, intents, asset references, and graph nodes from source tasks are uniformly marked with their source and kept read-only.
- Agent preview, task reports, and related detail views uniformly use the shared Markdown rendering component.
- LLM model configuration uses card and drawer interactions, and the model list supports independent scrolling within the drawer.
- Task pause no longer blocks main Agent conversation: the main Agent orchestration session is independent of task pause, and new messages can still be sent while paused (pause only terminates the current in-progress round).
- The default wall-clock duration of a single Worker run is changed from 600 seconds to 1200 seconds.

### Fixed

- Remove optimistic echo from the main Agent console and switch to pure server-side data rendering, fixing message disorder and content bleeding into other sessions when paused or when sending fails.
- Opening a main Agent session scrolls to the bottom by default and fully displays the last reply: the full content of the last reply is lazily expanded and automatically sticks to the bottom, no longer pushed off screen.
- Fix the issue where the main Agent session continues running after a task is paused, and where the orchestration Agent does not stop the main Agent synchronously when pausing a task.
- Fix the issue where the task status badge and action button states are out of sync when a task completes or when the Planner, Worker, or main Agent is actually running.
- Fix the issue where late blackboard writes or residual files may occur between Worker cancellation, task deletion, and concurrent writes.
- Fix Markdown preview failure, long text overflow in delete confirmation, mobile width, and misalignment of some task detail buttons.
- Add named termination reasons for all Agent cancellation paths; activity details can show the canceller, final state, number of run rounds, elapsed time, Token usage, and tool calls that did not return.
- Fix the race condition where the parent context may preemptively overwrite the named `shutdown` reason when the backend shuts down, as well as the garbled text caused by byte-based truncation of Chinese activity summaries.
- Fix the issue where cancellation events with partial streaming output lose the real termination reason, while preserving content already generated before cancellation.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

[Unreleased]: https://github.com/Autumn-27/ARTEX/compare/v0.3.10...HEAD
[0.3.10]: https://github.com/Autumn-27/ARTEX/compare/v0.3.9...v0.3.10
[0.3.9]: https://github.com/Autumn-27/ARTEX/compare/v0.3.8...v0.3.9
[0.3.8]: https://github.com/Autumn-27/ARTEX/compare/v0.3.7...v0.3.8
[0.3.7]: https://github.com/Autumn-27/ARTEX/compare/v0.3.6...v0.3.7
[0.3.6]: https://github.com/Autumn-27/ARTEX/compare/v0.3.5...v0.3.6
[0.3.5]: https://github.com/Autumn-27/ARTEX/compare/v0.3.4...v0.3.5
[0.3.4]: https://github.com/Autumn-27/ARTEX/compare/v0.3.3...v0.3.4
[0.3.3]: https://github.com/Autumn-27/ARTEX/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/Autumn-27/ARTEX/compare/v0.3.1...v0.3.2
