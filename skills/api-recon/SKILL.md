---
name: api-recon
description: Call this skill when collecting website API endpoints.
---

# API Recon (frontend endpoint reconnaissance)

Under the premise of **authorization**, discover as completely as possible: **backend APIs** (paths, methods, parameters, response bodies), **frontend routes**, and **UI feature trigger points** (Tabs, modals, table operations, etc.).

---

## Boundaries and prohibitions (Agent must-read · violation means overstepping)

This skill **only does API / parameter-surface reconnaissance**, not vulnerability hunting or exploitation.

### Task boundaries

| Scope | Allowed | Prohibited |
|---|---|---|
| **Target** | Enumerate paths, methods, parameters, routes, UI trigger points | SQLi/XSS/privilege escalation/brute force/fuzz vulnerabilities, packet-tampering attacks, destructive operations |
| **Auth** | Hook + stub/mock to bypass the **client-side** login gate | Asking the user for or guessing account credentials; attempting real login form submission |
| **Runtime** | Hook endpoints without credentials, use mock responses to get the SPA into the post-login shell | Flows that require a real backend session to continue |

### Credential-free dynamic analysis (Phase 3 default)

1. Via `preload.js` / `runtime_harvest.js`, **intercept and stub** bootstrap endpoints such as login, permissions, and menus;
2. For business query endpoints, return a mock body that is **structurally correct, with a successful business code, and data that may be empty**;
3. Enable the frontend to still render the post-login page without a backend or in a 401 environment, thereby triggering more XHR/fetch/WebSocket;
4. **Empty data, blank tables, and placeholder UI are all expected**—do not switch to real login or vulnerability testing because of this.

**In one sentence**: use mocks to open up frontend routes and component mounting, and **only record outbound requests**; what the backend returns does not matter, what matters is **which endpoints the frontend will still send**.

### Hard process prohibitions

| Prohibited | Alternative |
|---|---|
| Before Phase 1 completes, grep/curl/Read the main entry `index-*.js` to extract API paths | Run `OUTDIR/harvest_static.py` |
| Hand-writing scripts like `extract_apis.py` to replace harvest | Modify `OUTDIR/harvest_static.py` and rerun |
| Repeating the same grep/command after it fails ≥2 times | Change strategy: read tool_logs, modify harvest, check reference |
| Skipping gates A/B and directly running the original `scripts/` | Copy to OUTDIR and modify per target |
| Real username/password, OTP, OAuth, etc. for auth | stub/mock (see above) |
| Skipping stub on the grounds of 'getting real data' to do privilege escalation/injection testing | Only record outbound, within recon boundaries |
| Irreversible operations such as deletion, exporting sensitive data, batch writes | Same for coverage clicks |
| Runtime + dynamic enumeration not completed, yet claims all pages and endpoints obtained | See 'Definition of Done' or note limitations |
| Parameter trigger matrix + diff not completed, yet claims all parameters mastered | Phase 3b matrix + Phase 5 diff |
| Inferring required/optional from a single runtime sample | Multi-sample diff or validation rules/error reverse inference |

---

## Two-layer model + run modes

| Layer | Output | Limits |
|---|---|---|
| **Static** (JS bundle) | Full endpoint paths, route drafts, field candidates at assembly points | No HTTP methods; parameters require Phase 1b; misses runtime-concatenated URLs |
| **Runtime** (live session) | Method + body + response + dynamic URL + WS/SSE; multi-sample diff fills in parameters | Pages must actually render to send requests; a single sample is insufficient to determine required/optional |

| Run mode | Engine | Applicable |
|---|---|---|
| **depth** | `runtime_harvest.js` (Puppeteer) | API inventory, METHOD/params/response body, WS/SSE, reproducible batch runs |
| **coverage** | browser + `preload.js` | Click tabs/popups/tables, deeper feature coverage |
| **both** | depth first, then coverage | Most complete, most time-consuming |

**Parameter methodology** (no universal script): paths via harvest/regex; parameters via **anchor window expansion + UI binding chain + multi-sample diff + error reverse inference** (grep recipes see [reference.md](reference.md) section J).

---

## Definition of Done

All must be satisfied to claim recon is complete:

- [ ] **Static**: Phase 1 harvest produces `api_static.txt`, `routes.txt`, `js/`
- [ ] **Runtime**: at least one of depth or coverage; coverage/both must have **Hook active + dynamic enumeration loop**
- [ ] **Shell entry**: accessing business paths is not `/login` (watch for hash routing)
- [ ] **Parameters**: coverage/both completes parameter trigger matrix + `param_samples.json`; Phase 5 merges `params_merged.json`
- [ ] **Depth** (if module pages are blank): Phase 4 permission tree restoration and rerun until **module-level APIs** appear (not just locale/bootstrap)
- [ ] **Delivery**: Phase 5 outputs complete (see Phase 5 output table); `insert_assets` writes service and endpoint assets

---

## Scripts and gates

`scripts/` are reference templates only; **do not** run the originals directly and treat them as final results.

**Rule**: read first → modify per goal → write to `OUTDIR` (e.g. `recon/`) → record in `CHANGES.md`; if mismatched, rewrite per methodology, borrowing only the structure.

| Gate | When | Reference script → OUTDIR copy | Common required changes |
|---|---|---|---|
| **A (static)** | After Phase 0, before **first** harvest/spider run | `harvest_static.py` / `spider_mpa.py` | **Most sites' default regex works directly**; only change endpoint regex, webpack/Vite `publicPath`, MPA exclude/cookie when manifest/dialect mismatches |
| **B (runtime)** | After Phase 2, before running depth/coverage | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage keys, neutralize success values, stubs, login regex, api prefix, hash/history |

**SPA mandatory order** (not swappable; Phase numbering takes priority over 'explore first, then script'):

| Step | Required | Forbidden |
|---|---|---|
| After Phase 0 completes | Next Bash = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | curl/grep/Read main entry `index-*.js` (usually >500KB) |
| Gate A | Copy script → small edits as needed → **run immediately** | Manually extract API first, then decide whether to harvest |
| Before Phase 1 completes | `wc -l` to verify output; on 404, modify harvest and retry | Hand-write extract script; repeatedly grep undownloaded URLs |
| From Phase 1b onward | grep only `OUTDIR/js/*.js` | Use main bundle instead of harvest |

- ✅ Copy `harvest_static.py` → (optional) modify regex → **run immediately**
- ❌ curl main bundle → grep multiple times → write temp extract → harvest only at the end
- **MPA**: after Phase 0, next Bash = `python3 OUTDIR/spider_mpa.py ...`

---

## Tool and output constraints

| Constraint | Description |
|---|---|
| Large files | `index-*.js` >100KB **forbidden** to Read/grep into context; use OUTDIR scripts for batch processing |
| grep output | Must use `\| head -20` or `-m 5`; keep only path summaries in conversation, don't paste bundle fragments |
| Verification | Use `wc -l`, `ls \| wc -l`; don't Read entire directories |
| regex initial probe | Optional, ≤1 time, only ≤50KB small chunks or HTML; official static uses harvest as source of truth |
| reference | Recipes/templates/troubleshooting see [reference.md](reference.md), don't repeat full text inline |

---

## Execution roadmap

```
Phase 0 classification + OUTDIR
  → Gate A → Phase 1 harvest (★ run immediately ★)
  → Phase 1b parameter reverse engineering
  → Phase 2 auth three gates → config.json
  → Gate B → Phase 3 runtime + parameter matrix
  → Phase 4 permission tree (if needed) → rerun Phase 3
  → Phase 5 merge report + insert_assets batch-insert all discovered services and endpoint api assets; when inserting, never omit any discovered asset under any circumstances
```

Check in order; **do not proceed to the next Phase until the previous item is complete**.

1. [ ] **Phase 0**: Initial recon of SPA/MPA; create `OUTDIR` → [Phase 0](#phase-0--classification)
2. [ ] **Gate A + Phase 1**: copy scripts → **immediately** harvest → `wc -l` verify → [Phase 1](#phase-1--static)
3. [ ] **Phase 1b**: anchor window expansion + binding layer → `param_candidates.json` → [Phase 1b](#phase-1b--parameter-reversing)
4. [ ] **Phase 2**: three auth gates → `config.json` → [Phase 2](#phase-2--three-auth-gates)
5. [ ] **Gate B**: adjust runtime scripts → [Phase 3](#phase-3--runtime)
6. [ ] **Phase 3**: depth / coverage / both; confirm shell entry; parameter trigger matrix → `param_samples.json`
7. [ ] **Phase 4** (if needed): permission tree → patch stubs → rerun Phase 3 → [Phase 4](#phase-4--permission-tree-recovery)
8. [ ] **Phase 5**: merge outputs + report + `insert_assets` → [Phase 5](#phase-5--merge-and-report)

---

## Phase 0 — Classification

Fetch the entry HTML, **create `OUTDIR`** (do not modify `scripts/` inside the skill):

- **SPA**: empty shell + `<div id=app>` + chunk → Phase 1–5
- **MPA**: SSR + `<form>`, no endpoint bundle → after Gate A:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

Produce `forms.txt`, `links.txt`, `api_inline.txt`. If SPA has forms ≈ 0 → switch to Phase 1.

---

## Phase 1 — Static

Follow [Scripts and Gates](#scripts-and-gates) · [Tool and Output Constraints](#tool-and-output-constraints).

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest: parse HTML script → webpack/Vite manifest → download all lazy chunks → produce `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt`.

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- chunk count vs manifest: on 404, modify harvest and retry; do not manually curl each chunk
- `api_static.txt` too small → relax the endpoint regex within OUTDIR and rerun (see reference)

### Phase 1b — Parameter Reversing

paths come from Phase 1; parameter fields must be recon'd separately. For grep rules see [Tool and Output Constraints](#tool-and-output-constraints).

**Completion criteria**: able to answer for important endpoints — field names, transport location, type inference, whether required, sample values, confidence.

#### 1b.0 — Transport Form

| Form | Where params are | What to look at first statically |
|---|---|---|
| REST JSON | body + query | near path anchor `(params\|data\|body)\s*:\s*\{` |
| GraphQL | `variables` | gql template, `$page: Int` |
| Traditional form | urlencoded | `<form>`, `FormData` |
| File upload | multipart | `FormData.append` |
| Path parameter | `/user/:id` | route table + `useParams` / `$route.params` |
| Encryption/signature | wrapped into `sign`/`data` | Hook encryption function input (reference section D) |

Output: annotate each endpoint with `transport: query|json|form|graphql|encrypted`.

#### 1b.1 — Anchor window expansion

Use the known path as an anchor, expand the window to find the request-building object:

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| Wrapper layer | Parameter clue |
|---|---|
| axios instance | `data` / `params` |
| Unified request | interceptor injects global fields |
| OpenAPI client | generated method signature |
| React Query / SWR | hook second parameter |
| Vue composable | composable input |

Type residue: `yup`/`zod`/rules, `Form.Item name=`, embedded Swagger.

→ `param_candidates.json`：`{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — Binding layer

```
Form field → onFinish/handleSubmit → transform → API payload
```

| Binding source | Technique |
|---|---|
| Form submit | follow submit → transform → API |
| Table search | `getFieldsValue()` → `params` |
| Route | `:id` / `?tab=` |
| Interceptor | global `tenantId`, pagination, sign |
| Enum select | `options` → API enum values |

In DevTools call stack, trace upward from `fetch`/`XHR.send` to the request-building function.

#### 1b.3 — Three request-building questions (≠ Phase 2 three auth gates)

| Question | What to answer |
|---|---|
| **Assembly** | Where the payload is built, transform traces |
| **Validation** | required, pattern, enum |
| **Transport** | path / query / body / multipart / headers |

The interceptor gate (Phase 2) also reads globally injected fields (Authorization, `X-Tenant-Id`, sign).

#### 1b.4 — Connection with Phase 3

Candidate fields come from the static/binding layer; **required/optional/conditional dependencies** must be confirmed via the Phase 3 parameter matrix + diff + Phase 5 error reverse-engineering.

---

## Phase 2 — Three authentication gates

grep in `OUTDIR/js/` (with `head`), write to `config.json` (recipe see reference):

| Gate | Question | Keywords |
|---|---|---|
| **Render gate** | How to determine logged in? | `isLogin`, `getToken`, Cookie/localStorage |
| **Interceptor gate** | What triggers redirect to `/login`? | `response_code`, `errno`, axios interceptor |
| **Content gate** | Where do menus/permissions come from? | `menu`, `permission`, `role`, `acl`, `routes` |

Do not treat localStorage key names as credentials — must confirm from chunk/request chain.

**Exit = Gate B**: conclusions go into `config.json`, and modify `OUTDIR/runtime_harvest.js` / `preload.js`.

### Phase 2b — API observation (optional)

Use `preload.js` inside OUTDIR to confirm session key names, Authorization, nested API URLs:

| Config | Output |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | headers observation |
| `extractUrlsFromResponse: true` | sub-APIs in responses |
| `observe.storageReads/cookieReads: true` | backfill config |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

coverage exports each round: `__API_RECON_LOG__`, `__API_RECON_DETAIL__`, `__API_RECON_ROUTES__`, `__API_RECON_OBSERVE__`.

---

## Phase 3 — Runtime

Must have passed Gate B; follow [Boundaries and Prohibitions](#boundaries-and-prohibitions-agent-must-read--violation-means-crossing-the-line) · no-credential mock strategy.

`config.json` set `"runtimeMode": "depth" | "coverage" | "both"` (template see reference).

### Hook and stub (shared by depth + coverage)

| Layer | Scope | Purpose |
|---|---|---|
| L1 exact | auth/permissions/bootstrap stub | Pass first-screen authentication |
| L2 negative correction | All JSON responses | Not-logged-in code → success |
| L3 fallback | `/api` etc. not matched by L1 | Empty success body, prop up UI |

- **depth**: fake auth + `forward` to change business codes + `stubs`; traverse `routes` (hash/history); produce `runtime_api.json`
- **coverage**: **document-start** inject `preload.js` (CDP `addScriptToEvaluateOnNewDocument` or Userscript)

Verify: `window.__API_RECON_PRELOAD__` exists; business path does not return `/login`.

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — coverage dynamic enumeration (mandatory)

1. Main nav/sidebar — click each item, wait 1–3s for network
2. Tab — `role=tab`、`.ant-tabs-tab`
3. Tables — first row view/edit/details
4. Toolbar — export, filter, create (**avoid irreversible deletes**)
5. Each module entered — merge API/routes
6. SPA — controlled `pushState` for paths not covered by `routes.txt` (forbidden in MPA)

**Parameter trigger matrix** (mandatory): record once per module per operation type, **diff multiple samples**:

| Operation | Parameters usually added |
|---|---|
| List first screen | Pagination + default filters |
| Click search | keyword, filter |
| Advanced filter | More optional |
| Create/edit | Full entity |
| Batch/export/sort | `ids[]`, `exportType`, `sortField` |

**Under stub, outbound body/headers are still real** — defer to the request. Record → `scan_raw.json`, `param_samples.json`, `api_detail.json`.

- **Vue**：`neutralizeVueRouter: true` + document-start preload
- **React**: `routes.txt` + sidebar clicks + `pushState`
- **both**: first 3a depth, then 3b coverage

---

## Phase 4 — Permission tree restoration

**Trigger**: module page blank / each route only bootstrap (e.g. locale) → content gate not passed.

| Symptom | Meaning |
|---|---|
| Shell entry succeeded | Render gate + interceptor gate passed |
| Sidebar missing items/blank on click | stub shape or incomplete permission codes |
| Same API for every route and very few | `v-if permission` not passed |
| `routes.txt` far fewer than bundle | Must be completed from auth module |

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
```

Typical chain: `role_permissions` (flat codes) + `permissions/all` (tree) → `getResultTree` → `userRouteAuth[CODE].url`.

```bash
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Intermediate outputs: `route_map.json`, `userRouteAuth.json`, `permissions_tree.json`, `*_stub.json`, `perm_codes_all.txt`.

stub check: outer `response_code` matches interceptor gate; flat codes aligned with tree; `routes` covers all links in `route_map`.

After updating `config.json`, **rerun Phase 3**. For large SPAs, adjust `waitUntil`, `routeTimeout`, `perRouteMs` (see reference A3/I sections).

---

## Phase 5 — Merge and Report

### Output Table

| File | Phase | Content |
|---|---|---|
| `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` | 1 | Static bundle and paths |
| `param_candidates.json` | 1b | Static parameter field candidates |
| `config.json` | 2 | Three gates + runtime config |
| `runtime_api.json` | 3a | depth detailed recording (incl. WS/SSE) |
| `param_samples.json`, `scan_raw.json`, `api_detail.json` | 3b | Multiple samples, click logs, detail |
| `route_map.json` etc. | 4 | Permission tree intermediate files (if executed) |
| `params_merged.json` | 5 | Merged parameter fields + confidence |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | Routes, APIs, params, feature points, limitations |
| **insert_assets** | 5 | Write all service and endpoint assets to the asset library |

### 5b — Parameter Merge

Diff from `param_samples.json`, **no generic merge script**. Confidence rules see reference J7 (high/medium/low/pending trigger).

### 5c — Error Reverse Inference

Within authorization scope, can send incomplete requests to read 400 (**this is parameter recon, not vulnerability testing**): `field 'x' is required`, enum errors, etc. Note `data` wrapping, `variables`, and pre-encryption `bizData`.

The report must state: runtimeMode, static/runtime API counts, parameter confidence, uncovered modules, and a `CHANGES.md` summary relative to the reference script.

Recommended structure for `site_map.json`:

```json
{
  "site": "https://example.com",
  "runtimeMode": "both",
  "appType": "vue-spa",
  "routeGuardStrategy": ["nav-neutralize", "L1-auth", "L2-patch", "forward"],
  "apisFromStatic": [],
  "apisFromRuntime": [],
  "apis": [],
  "params": [{ "method": "POST", "path": "/api/user/list", "transport": "json", "fields": [] }],
  "frontendRoutes": [],
  "routesVerifiedByClick": [],
  "featuresTriggered": [],
  "limitations": ""
}
```

For more fields and grep recipes, see [reference.md](reference.md).

---

## General Notes

- **Framework-agnostic**: webpack/Vite/Angular lazy load methods are the same
- **Transport**: REST/JSON, GraphQL, WebSocket, SSE; gRPC-web is out of scope
- **SSR**: client-side fetch can be recorded; RSC/Server Actions are not fully enumerable
- **Blind spots**: JSVMP, WASM, HMAC/mTLS strong validation → static + note limitations
- **Parameter blind spots**: conditional linkage, hidden params, WASM packet assembly → 'pending trigger'/'unreachable'
- **Static is the safety net**: when runtime is blocked, static can still enumerate endpoints

---

## Additional Resources

- Grep recipes, `config.json` template, troubleshooting, Hook, parameter reverse-engineering section J, site_map template: **[reference.md](reference.md)**
For reference script paths, see the [Scripts and Gates](#scripts-and-gates) table
