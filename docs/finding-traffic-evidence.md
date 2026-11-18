# Traffic evidence for findings

A finding can have more than one traffic record. In the finding's **Linked traffic** section, select records across pages, set each record's role and notes, change the order, or remove a link. From the traffic page, you can also select several records and link them to one existing finding. Inherited evidence is read-only. Make changes in the source task.

## Automatic linking and reports

Automatic traffic linking is off by default. The setting is `agent_traffic_binding` in `/api/settings`. Enabling it takes effect on the agent's next turn and can increase token usage because inspecting requests and responses adds tool calls and prompt content. When disabled, the automatic linking parameters, tools, and instructions are hidden. New automatic link requests from sessions already running are also rejected. Manual linking, traffic capture, and access to saved evidence and exports remain available.

Once a finding is saved, its report can use existing traffic records. The evidence handoff includes the verification commands and key output already collected, along with any known traffic IDs and their roles. With automatic linking enabled, the report process checks the recorded traffic, links the relevant snapshots, then saves the report against the latest evidence version. With the setting disabled, the report can still use snapshots linked manually.

Traffic attachments are optional. Non-HTTP findings and cases with missing or incomplete recordings can use other evidence, such as existing logs or command output. Explain a missing recording where useful. An explicit attachment requires a valid ID and a complete body for every record. If one fails, the whole attachment batch rolls back. When attachments are submitted with a new finding, failure also rolls back the finding. Adding the same snapshot twice preserves the existing link and note.

## Identifiers, roles, and versions

`traffic_refs` is an optional ordered list on `report_finding`. Its order sets the initial evidence order.

```json
{
  "traffic_refs": [
    {"traffic_id": "<existing traffic ID>", "role": "baseline", "note": "Normal account request"},
    {"traffic_id": "<another existing traffic ID>", "role": "proof", "note": "Recorded request supporting the finding"}
  ]
}
```

The evidence roles are `baseline` for a normal comparison, `proof` for evidence of the finding, `verification` for additional verification, and `supporting` for supplementary material. The default is `supporting`. A domain or timestamp can help filter recorded traffic. Neither establishes which task owns a record. Use an actual record ID and avoid repeating activity solely to obtain a recording.

The first response line remains `finding recorded: <exploration node ID>`. The JSON that follows includes the independent finding record ID (`finding_id`), its exploration node ID (`finding_node_id`), and a summary of the links. The IDs serve different purposes:

- `get_finding_traffic(finding_id)` takes the **finding record ID** and returns the ordered list and its `version`. Supply `binding_id`, `side=request|response`, `offset`, and `length` to read a segment of up to 8,192 bytes.
- `update_finding_report` still takes the **exploration node ID** in its `finding_id` field. Supply the actual version read as `evidence_version`. If the evidence changes before the report is saved, the write fails. Read the new version and regenerate the report.
- A legacy write without a version does not establish that the report covers the current traffic evidence. Changing a link, note, role, or the ordering marks the report as due for an update.

An existing caller can submit links immediately through `traffic_refs` or reference previously saved links through `evidence_hint_id`. The hint must belong to the current task. Inherited hints cannot supply these references. The system does not infer links from domains, timestamps, or browsing history. A failed submission leaves no partial finding or report job.

Use `bind_finding_traffic(finding_id, traffic_refs)` to attach evidence to an existing finding without creating a duplicate. The finding and node detail responses include both `finding_id` and `finding_node_id`. The legacy `id` field still refers to the exploration node.

At startup, existing schemas receive the new optional properties. Custom bindings, descriptions, prompts, and enablement settings are preserved. Reports use evidence available in the task context. A missing recording need not hold a report open.

## HTTP API

The base path is `/api/exploration/findings/{finding_id}/traffic`. Use the independent finding record ID. Existing authentication requirements apply. `context_task` checks visibility and enforces read-only access to inherited evidence.

| Method and relative path | Request or response |
| --- | --- |
| `GET` | Ordered summaries, current evidence version, and the version used by the report |
| `POST` | Append the entire batch in `{"traffic_refs":[...]}` |
| `PATCH /{binding_id}` | Update the role or note, e.g. `{"version":1,"role":"proof","note":"Observed response"}` |
| `DELETE /{binding_id}` | `{"version":1}` |
| `PUT /order` | `{"version":1,"binding_ids":["2","1"]}`. Supply the complete list. |
| `GET /{binding_id}` | Snapshot metadata and a bounded body preview |
| `GET /{binding_id}/body` | Read a segment using `side`, `offset`, and `length`. Use `download=1` for the complete original bytes. |

Version conflicts, mismatched sets of IDs during reordering, and writes during archiving return `409`. Writes to inherited evidence return `403`. Missing links and links belonging to another finding return `404`. Read and integrity failures are reported explicitly.

## Storage and migration

The idempotent PostgreSQL migration at startup adds `traffic_evidence_snapshots`, `finding_traffic_bindings`, `findings.evidence_version`, and `report_evidence_version`. Both version fields default to zero. The migration does not infer links from historical text.

A snapshot records the original traffic ID, capture time, URL, method, status, request and response headers, body lengths, and SHA-256 hashes. Bodies are stored by hash at `<data>/evidence/blobs/<first-two-hash-characters>/<hash>.bin`, separate from the disposable `data/traffic` directory. Findings can share snapshots and bodies. Snapshot content is immutable. An integrity mismatch causes the read or export to fail.

The full bodies, including large blobs and records in the legacy layout, are read under the source traffic write lock. Files are persisted and verified before one PostgreSQL transaction writes the finding, node relationships, snapshots, and links. Notifications follow the commit. A failure can leave an unreferenced file, but no partial database record.

PostgreSQL advisory lock `7337741004` coordinates evidence files and database references. A task row lock prevents edits once archiving is queued. Restore holds the evidence lock from installation of the bodies through the metadata commit. Deleting a finding also removes its links.

The hourly cleanup job waits at least 24 hours before reclaiming unreferenced content and excludes operations still in progress. Routine traffic cleanup leaves the evidence directory alone. Back up PostgreSQL and `data/evidence` together.

## Exports and archives

Markdown includes the ordered evidence list and version. JSON includes metadata. CSV includes the evidence count and binding IDs. The Markdown ZIP includes a report for each finding and the following files:

```text
evidence/<finding_id>/<binding_id>/
  manifest.json
  request.http
  response.http
  request.bin
  response.bin
```

The Markdown uses relative links to the messages. Before serving a download, the exporter copies attachments, verifies their hashes, compresses and syncs the archive, then reads every ZIP entry to verify its CRC. A missing or corrupt attachment fails the entire download. The binary files preserve the original bytes.

Version 3 archives collect snapshots and bodies by finding link. They do not require the original traffic records or a domain match. Hot data is removed only after the package passes verification. Shared evidence is retained. Restore verifies and installs bodies before restoring metadata and bindings in a transaction. A failed restore can be retried. Versions 1 and 2 remain readable, with missing new fields set to zero.

## Verification and limits

Use a separate, newly created PostgreSQL test database for each package and set `ARTEX_PG_DSN` to its connection string. This keeps old task and model fixtures from starting background work. Run the relevant package suites and check that database configuration errors have not caused tests to skip.

```sh
# Set ARTEX_PG_DSN to the isolated database for each package.
go test ./<package> -count=1
go test -race -p 1 ./evidence ./db ./agent ./server -run 'TestEvidence|TestFindingTraffic|TestFindingEvidence|TestReportFindingAtomicContract|TestTaskArchive'
```

Frontend checks include `npx tsc --noEmit`, Biome checks on affected files, a Webpack build, and a static export with `NEXT_EXPORT=1`. Use a separate cache directory to avoid overwriting the development server's cache.

Local acceptance testing uses a separate port, controlled HTTP and domain-based HTTPS fixtures, and a temporary data directory. It covers both linking entry points, cross-page selection, ordering, notes, errors, inherited read-only access, and downloads. The archive round trip includes deleting the original traffic record, exporting, archiving, reclaiming the hot body, restoring, and checking its hash.

A global lock coordinates evidence operations in this release. Large batches, exports, and slow downloads can delay other evidence operations. Missing recordings or incomplete bodies cannot be reconstructed. This feature does not change traffic capture settings or certificate handling for HTTPS connections made directly to an IP address.
