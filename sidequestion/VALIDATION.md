# `/btw` validation record

Date: 2026-09-10. Branch: `codex/btw-side-question`. Baseline: `8dae851b9b622f2ff2631f332fde9719d0b16fba`.

Used a separate PostgreSQL test database and data directory; real model credentials were injected only into the isolated test environment, were not written into code or this record, and did not modify the product default model. Go 1.26.3, norma v0.3.6, Next.js 16.2.9.

The actual model conversation, returned objects, engineering assertions, and the original Qwen review text are saved in [validation-2026-09-10.json](validation-2026-09-10.json), which contains no API credentials.

## Engineering checks

| Scope | Result | Evidence |
| --- | --- | --- |
| Structured messages, deep copy of tool arguments | Passed | `TestCheckpointDeepCopyAndBoundaries` |
| Summary / compaction requests do not overwrite, complete replies and terminal-state publishing, partial replies excluded | Passed | `TestCheckpointDeepCopyAndBoundaries`, `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Actual model pool member identity | Passed | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Tool pairing, 20-group replay, budget trimming and over-limit errors | Passed | `TestBuildRequestCompactionToolPairingAndBudget` |
| Main and side-path parallelism, bidirectional cancellation isolation | Passed | Blocking Provider, `TestMainSideConcurrencyAndIndependentCancellation` |
| No tool execution, streaming / non-streaming, existing usage on failure | Passed | `TestServiceNoToolsAndUsageOnFailure` |
| Real norma ChatAgent + local Read tool, main transcript / activity isolation | Passed | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`, streaming and non-streaming subtests |
| Persistence, pagination, idempotency, partial answers preserved across restart | Passed | `TestSideHistoryIdempotencyPagingAndRecovery` |
| Clear vs. late-write race, parent resource deletion, version comparison | Passed | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent / Worker archive and restore, v1/v2/v3 | Passed | `TestSideTaskArchiveVersions` |
| Three parent interfaces, authentication, resource ownership, Worker logical deletion | Passed | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`, `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Side path allowed on busy main session, independent SSE reconnect / disconnect, cancellation, clear | Passed | `TestSideHTTPBusyIsolationClearAndReconnect` |
| 1 per parent session / 4 global concurrency | Passed | Two `TestSideHTTP…` cases |
| Snapshot persisted before submission, follow-up after restart, old sessions cannot forge snapshots | Passed | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Reject continuation after cached config is deleted or model changes | Passed | `TestSideRejectsDeletedOrChangedCachedProfile` |
| Cancel before archive and wait for final answer and usage to be persisted | Passed | `TestSideTaskDrainPersistsBeforeArchive` |
| Record usage and side-path attribution only once when streaming consumer cancels early | Passed | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| Worker / deadline runtime context restored on restart continues publishing new snapshots | Passed | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| Race check for related packages | Passed | Commands below |
| TypeScript and production build | Passed | `npx tsc --noEmit`, `npm run build` |
New frontend modules Biome | Passed | `biome check`, 3 new modules |

After configuring `ARTEX_PG_DSN` in a separate disposable database, the automated checks can be reproduced (do not point to the production database):

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

The full Go regression is not all green: two existing tests in the `server` package fail during temp directory cleanup, both reporting `TempDir RemoveAll … directory not empty`:

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

After exporting the source from the above unmodified baseline and rerunning the `server` package in the same isolated environment, these two cleanup failures also reproduce. The baseline run additionally showed a target node count assertion failure in `TestCoreTaskLifecyclePG`; the final modified `server` regression did not have this assertion failure. Other packages passed, and the bypass-related test cases and race checks passed this time. Baseline issues were not marked as passing this acceptance, nor were existing assertions modified to hide issues.

The Next.js build output has existing multiple lockfile / workspace root inference warnings; the build completed and all pages were generated successfully.

## Browser checks

Using Codex In-app Browser, connected to an independent local Go service and Next.js dev server. The following manual automated operations were completed on desktop and a 390 × 844 narrow screen, checking screenshots and browser logs:

- Entered `/btw` while a normal chat was running; the main content and bypass were displayed simultaneously; the desktop sidebar was normal.
- Consecutive follow-up questions; after the bypass stopped, the generated portion was retained; the main flow continued.
- When the panel was closed, the request continued; after reopening, the completed answer was restored; after refreshing the page, an empty `/btw` restored history.
- The narrow-screen Drawer's input, buttons, history, and close operations were normal, with no horizontal overflow.
- Clearing used a confirmation dialog; after clearing, history disappeared, and the main transcript and snapshot were retained.
- The task MainAgent and two Workers were asked questions separately and switched; Agent tabs and history did not cross-talk.
- The blocking local model fixture kept the Worker running; `/btw` was submitted from the Worker's main input box, and after stopping the bypass the Worker still showed live running and its own pause button, and the bypass saved the partial answer.
- Browser error / warning logs were empty.

The controllable fixture was used to precisely verify concurrency timing, without relying on the real model's output speed. During debugging, two Worker runtime checks did not form a valid concurrency window (task already ended / answer ended early); after fixing the fixture, it was redone and passed; these initial operations were not recorded as valid passes.

## Real model conversation

Prioritized probing `grok-4.6`, OpenAI-compatible endpoint `http://127.0.0.1:12580/tingly/openai`. The probe returned HTTP 200, with model name `grok-4.6` and `READY`, taking 2.82 seconds. The first choice was available, so the Tingly `glm` or Zhipu `glm-5.3` fallback chain was not enabled; these two fallback services were not verified this time.

| Scenario | Actual result |
| --- | --- |
| Asked about assets, goals, and markers while the main session was running | Returned `redhaze.top`, homepage reading and summary goals, `BTW-REAL-0910`; bypass completed, 16.97 seconds |
| Asked about tool evidence after the main session finished reading the homepage | Correctly cited WebFetch 200, curl redirects 301 → 302 → 200, page title; 7.24 seconds |
| Bypass requested Bash to create a test file | Refused to execute, target file was not created; 7.74 seconds |
| Bypass after completion does not change the main context | Main transcript SHA-256 remained consistent with the main activity record; bypass tool execution count was 0 |
| Follow-up questions after actually stopping / restarting the Go service | Retained the previous 3 bypass history entries, answered assets, markers, and title directly from the persisted snapshot, and did not rerun the main Agent |
| New session uses Grok non-streaming configuration | Correctly answers asset and `ATOMIC-0910`; returns and saves usage: input 11734, output 138, cache_read 11520 |

The asset case's main session uses WebFetch and Bash/curl to read the public homepage; the landing page is `https://id.redhaze.top/home`, titled 'RedHaze Group · Global Integrated Group Portal'. Bash caches the response in a local test file; no remote writes were performed. This fact is verified separately from 'the bypass executed no tools'.

Main transcript checksum: `e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`.

**Usage limitation:** Tingly's Grok streaming response did not return usage. Separately, `stream_options.include_usage=true` was sent directly to verify: HTTP 200, 12 data frames, 0 usage frames. Therefore the 0 in the streaming test means the endpoint did not provide usage, and cannot be interpreted as no billing. Non-streaming usage and the fixture's failure / cancellation usage are all saved correctly.

## Qwen Review

Review model `qwen-flash`, OpenAI-compatible endpoint `https://dashscope.aliyuncs.com/compatible-mode/v1`, HTTP 200. Provided the first three real bypass conversations, main session tool evidence, and engineering assertions; returned `verdict: accept`, `concerns: []`, considering the answer consistent with the asset, marker, and page-read evidence, and the bypass tool rejection compliant with constraints. Review usage: prompt 6625, completion 312, total 6937.

This Qwen review scope does not include the later-appended service restart and non-streaming tests. Qwen's summary of 'no writes' is too broad: the main session curl did indeed create a local response temp file, as explicitly recorded above. Concurrency, zero tool execution, and transcript isolation are judged by engineering assertions; model review only assists in evaluating answer quality.
