# api-recon — Reference Manual

Grep recipes, `config.json` templates, and troubleshooting. All greps are executed against the `js/` directory. When a bundle is a single line, you can first run `js-beautify` or `sed 's/}/}\n/g'`; usually a raw grep with a context window is sufficient.

## Script Notes

All files in `scripts/` are **reference templates** and must be adjusted for the target site before execution. Typical change points:

| Script | Common items to adjust |
|---|---|
| `harvest_static.py` | endpoint regex, webpack/Vite manifest parsing, micro-frontend publicPath, retry/concurrency |
| `runtime_harvest.js` | neutralize field names and success values, stub matching rules and body structure, routes source, WS recording, `waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`, L1 stubs, `neutralize.fields`, `apiPattern`, whether to enable L3, `recordDetail`, `observe.*`, `neutralizeVueRouter` |
| `spider_mpa.py` | `--exclude` destructive links, cookie, depth/max, same-domain filtering |
| `extract_route_map.py` | `routeMap` / `routeLink` regex, KEY naming patterns |
| `build_perm_tree.py` | `userRouteAuth` parsing, `ROOTS`/`PREFIX_PARENT` hierarchy heuristics, stub outer field names |
| `config.json` | unified entry point for all the above site-specific parameters |

Adjusted files are recommended to be placed in the task working directory (e.g. `recon/`), and the report should note the specific changes relative to the reference scripts.

---

## A. The Three Reverse-Engineering Gates

### A1. Render Gate — 'How to determine that you are logged in?'

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

Find the chain `isLogin = f(getUser())` → `getUser = decode(storage.read(KEY))`, and determine the **storage key**, **container** (Cookie vs localStorage), and **encoding**:

| Encoding | config forgery method |
|---|---|
| Plaintext string / `"1"` / token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | unsigned/`alg:none` JWT, or signed with a key found in the bundle |
| Encrypted (SM2/AES/RSA) | Find the hardcoded key; if the render gate only needs a decodable blob, you can forge; otherwise fall back to static |

→ Write to `cookies` / `localStorage`.

### A2. Interceptor Gate — 'What triggers a redirect to /login?'

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(not logged in|please log in again|login expired|unauthorized|login invalid|authorization|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

Determine: **field name**, **success value** (usually `0` or `200`), **failure value that triggers the redirect**. Verify with a junk session:

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

→ Write to `neutralize.fields` + `neutralize.success`.

### A3. Content gate — 'Where do menus/permissions come from?'

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**Two layers of data** (common enterprise backends):

| API | Typical payload | Consumer |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | Route guards, button-level ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | Sidebar menu rendering |
| `userRouteAuth` in bundle | `{ CODE: { url, name? } }` | code → frontend path |
| `routeMap` in bundle | `{ KEY: { name, link } }` | Alias resolution (webpack `o.DASHBOARD`) |

Read the consumer code to confirm: how `getResultTree(tree, permissions)` filters, and which field `v-if` / `hasAuth(code)` checks.

**Manual forge** (small sites): build a permissive payload → `stubs`.

**Full permission tree restoration** (large sites, sidebar/submodules still blank): see **Section I**.

---

## B. config.json template

```json
{
  "baseUrl": "https://target/",
  "runtimeMode": "both",
  "chromium": "/usr/bin/chromium",

  "cookies": [
    { "name": "auth", "value": "b64json:{\"id\":1,\"username\":\"admin\",\"role\":\"admin\",\"func\":{},\"permissions\":[\"*\"]}" }
  ],
  "localStorage": { "token": "faketoken", "isLogin": "1" },

  "neutralize": {
    "fields": ["response_code", "code", "errno", "ret", "status"],
    "success": 0,
    "flags": { "success": true, "message": "ok" }
  },
  "forward": true,
  "loginUrlPattern": "/login",
  "apiPattern": "/api/|/rest/|/graphql",

  "mockTier": "L1+L2",
  "recordDetail": true,
  "observe": {
    "storageReads": false,
    "cookieReads": false,
    "xhrHeaders": true
  },
  "neutralizeVueRouter": true,
  "stubs": [
    {
      "match": "permissions/all|/menu|role_permissions",
      "body": {
        "response_code": 0, "code": 0,
        "data": {
          "permissions": ["*"],
          "menus": [
            { "name": "dashboard", "path": "/dashboard", "show": true, "children": [] },
            { "name": "alert", "path": "/alert", "show": true, "children": [] }
          ]
        }
      }
    }
  ],

  "explore": {
    "clickTabs": true,
    "clickTables": true,
    "pushStateFallback": true,
    "maxMenuItems": 50
  },

  "routes": ["/dashboard", "/alert", "/asset", "/device", "/report", "/config", "/system"],
  "waitMs": 1500, "perRouteMs": 900, "headless": true,
  "waitUntil": "domcontentloaded",
  "routeTimeout": 12000,
  "proxy": "",

  "captureResponses": true, "recordWs": true, "respMax": 600
}
```

Field descriptions:
- `runtimeMode`：`depth`（Puppeteer）、`coverage`（browser MCP）、`both`
- `cookies[].value` prefix: `b64json:` → base64(JSON); `json:` → raw JSON; no prefix → literal
- `forward: true` forwards real requests and rewrites code fields; `false` fully offline stub
- `mockTier`: coverage mode preload enable level, e.g. `L1+L2`, `L1+L2+L3`
- `routes` comes from `routes.txt`; after forging menus the harness auto-appends `<a href>`
- `captureResponses` / `recordWs` only valid in depth mode
- `waitUntil`: use `domcontentloaded` for large SPAs to avoid `networkidle2` hanging
- `routeTimeout`: per-route `page.goto` timeout (milliseconds)
- `proxy`: Puppeteer `--proxy-server`; can also set `HTTP_PROXY` / `HTTPS_PROXY`

### B1. Dual stub template (role_permissions + permissions/all)

```json
"stubs": [
  {
    "match": "role_permissions",
    "body": {
      "response_code": 0,
      "data": {
        "permissions": ["MONITOR", "MONITOR_ALERT", "THREAT", "ASSETS_RISK"],
        "role_type": "SUPER_ADMIN"
      }
    }
  },
  {
    "match": "permissions/all",
    "body": {
      "response_code": 0,
      "data": [
        {
          "code": "MONITOR",
          "position": 1,
          "children": [
            { "code": "MONITOR_ALERT", "position": 1, "children": [] }
          ]
        }
      ]
    }
  }
]
```

Outer field names (`response_code` / `code` / `data`) must match the A2 interceptor gate; `permissions` must cover all leaf codes in the tree.

---

## C. coverage mode: preload configuration

Edit the `CONFIG` object at the top of `scripts/preload.js`, or replace before CDP injection:

```javascript
const CONFIG = {
  loginPathRe: /\/(login|signin)(\/|$|\?)/i,
  mockTier: 'L1+L2',
  forward: true,
  recordDetail: true,
  extractUrlsFromResponse: true,
  neutralizeVueRouter: true,
  observe: { storageReads: false, cookieReads: false, xhrHeaders: true },
  neutralize: { fields: ['response_code', 'code'], success: 0 },
  stubs: [ /* same as config.json stubs */ ],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

Verify: `window.__API_RECON_PRELOAD__ === true` and pathname is stable.

Export recording results:

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. preload / runtime Hook capabilities

Built-in browser Hook capabilities and coverage of preload (coverage) and runtime_harvest (depth):

| Hook capability | Value for API discovery | Coverage |
|---|---|---|
| Hook fetch / XHR.open | Record request URL/method | ✅ `recordDetail` + `__API_RECON_LOG__` |
| Hook XHR.setRequestHeader | Discover headers like Authorization | ✅ `observe.xhrHeaders` |
| Hook localStorage/cookie reads | Confirm session key names | ⚠️ Optional `observe.storageReads/cookieReads` |
| Vue get routes | Complete frontendRoutes | ✅ `__API_RECON_ROUTES__` (loaded routes) |
| Neutralize Vue route guards / block login redirects | Open up modules to trigger APIs | ✅ `neutralizeVueRouter` + native redirect neutralization |
| React get routes | Complete routes | ⚠️ Static + clicks; no dedicated Hook |
| Page redirect blocking (login path) | Stay on page for analysis | ⚠️ Only blocks login path, avoids blocking business navigation |
| Hook crypto libraries (CryptoJS/SM etc.) | Encrypted params → plaintext API body | ❌ Must manually Hook crypto function inputs; write conclusions to config |
| Anti-debug bypass | Otherwise runtime can't record APIs | ❌ Must handle manually; static still usable |

---

## E. Endpoint extraction regex (when static is too few)

Relax `extract_endpoints` in `harvest_static.py`, or manually:

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. Troubleshooting

| Symptom | Cause → Fix |
|---|---|
| Few static APIs | endpoint dialect mismatch → relax regex (section D) |
| chunk count ≪ manifest | CSS-only or undeployed chunks; 404 already retried |
| runtime still shows login page | Render gate error → recheck A1: key names, container, encoding, domain |
| Shell loads but module blank | Content gate → forge menu (A3); `routes` path may be wrong |
| Only bootstrap/locale per route | Incomplete permission codes → section I permission tree restoration; check both `role_permissions` + `permissions/all` stubs |
| Sidebar has items but subpage blank | tree missing intermediate nodes or code inconsistent with `userRouteAuth` |
| Every API redirects to login | Interceptor gate → confirm `neutralize`; nested fields require extending walk logic |
| WS frame is 0 | subscribe only after user interaction; increase `perRouteMs` |
| Response body empty | real response only when `forward: true` |
| Chromium missing | install chromium or set `config.chromium` / `CHROMIUM` |
| Many mocks but still returns to login | Hook too late or missing `location.href` setter → document-start + preload |
| List all empty | L3 empty array is normal; keep clicking Tab/settings/details |
| Mistakenly treating Redux action as route | filter internal paths containing get/set/change/clear/toggle/upload |
| Vue still redirects to login | preload not at document-start → change injection timing; or manually clear guards when `neutralizeVueRouter: false` |
| Response has URL but not in log | enable `extractUrlsFromResponse`; or manually extract from `__API_RECON_DETAIL__` |
| Don't know Authorization header name | enable `observe.xhrHeaders` or check request headers in DevTools |
| runtime extremely slow / timeout | change to `waitUntil: domcontentloaded`; lower `routeTimeout`; don't use `networkidle2` |
| Proxy connection failed | check `proxy` / environment variables; Puppeteer and curl proxy ports must match |

---

## G. hardened targets

When the server progressively validates the session (unforgeable signed cookies, server-rendered and unstubbable menus), runtime will get stuck at the shell. Expected behavior:

- **Static is enough for endpoint enumeration** — module paths are in the code
- If authorization allows, run the same harness with a **real session**: `forward: true`, no neutralize needed, capture real methods/params/responses

---

## H. Single-task checklist

1. Confirm authorization scope
2. **Read** `scripts/harvest_static.py` → adjust for target → run → review `api_static.txt`, `routes.txt`
3. **Phase 1b**: path anchor window expansion + binding layer → `param_candidates.json` (Section J)
4. Reverse A1/A2/A3 → write site-specific `config.json`
5. **Read and adjust** `runtime_harvest.js` / `preload.js` before executing
6. `runtimeMode=depth`: `npm install` → run adjusted harvest script
7. `runtimeMode=coverage/both`: document-start inject adjusted preload → browser MCP dynamic enumeration + **parameter trigger matrix**
8. Module not rendering → **Section I permission tree restoration** → patch stubs → rerun
9. Parameter multi-sample diff + error back-inference → `params_merged.json`
10. Merge → `site_map.json` + `api_merged.txt`, honestly annotate coverage, gaps, and script change points

---

## I. Permission tree restoration (Phase 4 deep dive)

Use when forge's simple `menus: [{ path, show: true }]` is ineffective and submodules still don't mount.

### I1. Locate the auth module

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

Record: **permission API path**, **response field names**, **consuming chunk file names**.

### I2. Extract routeMap

```bash
python3 scripts/extract_route_map.py recon/js recon/
# Produces recon/route_map.json
```

If `[!] no routeMap pattern found`: relax the regex in `extract_route_map.py`, or grep manually:

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. Build permission tree + stub

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Script logic:
1. Parse `userRouteAuth={MONITOR:{url:...},...}` (including webpack alias `He=o.DASHBOARD`)
2. Use `route_map.json` to resolve alias → real path
3. Infer parent by code prefix (`MONITOR_ALERT` → `MONITOR`)
4. Output `permissions_tree.json`, `permissions_all_stub.json`, `role_permissions_stub.json`
5. With `--config`, automatically write to `config.json`'s `stubs` and extended `routes`

**Adjust per target** (at the top of the script):
- `DEFAULT_ROOTS`: list of top-level module codes
- `DEFAULT_PREFIX_PARENT`: `PREFIX_` → parent mapping
- `DEFAULT_EXTRA_PARENT`: orphan nodes with non-prefix relationships

### I4. Validate stub consistency

```bash
# permissions count should ≈ userRouteAuth entry count
wc -l recon/perm_codes_all.txt
# routes should cover all links in route_map
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. Rerun runtime and compare

```bash
node recon/runtime_harvest.js recon/config.json
# Compare the number of entries in runtime_api.json before and after forge; check whether module APIs appear for /attack, /asset, etc.
```

| Before forge | After forge (success) |
|---|---|
| Same 3–5 bootstrap entries per route | Different routes trigger different module APIs |
| Only `/api/locale/language` | Module endpoints like `/api/web/...` appear |
| Single-digit routes in `routes.txt` | `routes` 80–110+ from route_map |

### I6. When it still fails

- **coverage mode**: click the sidebar + Tabs; permission gating may only request after interaction
- **stub fields**: compare the nesting of the real API (curl + real session) with the stub
- **extra guards**: grep for button-level checks like `hasPermission|checkRole|func.`, extend `role_permissions.permissions`
- **static fallback**: module API paths remain in `api_static.txt`; runtime only fills in METHOD/body; parameters keep `param_candidates.json` + recorded samples

---

## J. Parameter reverse engineering (Phase 1b / 5b / 5c)

**Methodology, not a generic script.** Find paths with regex; find parameters with anchor window expansion + UI binding chain + multi-sample diff + error back-inference.

### J1. Anchor window expansion — find the request-building object from the path

```bash
# Use the known path from Phase 1 as the anchor
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. Wrapper layer and transport form

```bash
# axios / unified request
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData / multipart
grep -rhoaE 'FormData|\.append\(' js | head

# path parameters
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. Validation gate — required / format / enum

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. Binding layer — form → API

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

Runtime fill-in: DevTools → Network → request → **Initiator** (call stack), trace upward from `fetch`/`send` to the request-building function.

### J5. Encrypted parameters

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**Don't guess fields on the ciphertext** — hook the encryption function's **input parameters**, record the plaintext payload before encryption; write conclusions to `config.json` / `param_candidates.json`.

### J6. Parameter trigger matrix (mandatory in Phase 3)

Record once per operation for each module, diff the request body/query:

| Operation | Focus |
|---|---|
| List first screen | Pagination defaults |
| Search | keyword, filters |
| Advanced filter | optional field |
| Create/Edit | full entity |
| Batch/Export | `ids[]`, `exportType` |
| Sort/Pagination | `sortField`, `order` |

Produce `param_samples.json`: `[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. Confidence rules

| Confidence | Condition |
|---|---|
| **High** | Static callsite + runtime ≥2 consistent samples |
| **Medium** | Static only, or only 1 runtime |
| **Low** | Inferred from response/error, not re-verified |
| **Pending trigger** | Statically known field, UI/permissions not reached |

### J8. Scenario quick config

| Scenario | Order |
|---|---|
| REST list page | J1 group package object → J6 four diffs → J3 rules |
| Create/Edit form | J3 Form name → J4 submit chain → runtime submit + intentionally leave blank to see 400 |
| GraphQL | J2 variables declaration → record variables for each operation at runtime |
| Encrypted body | J5 Hook input params → fields before encryption are the real params |

### J9. Mapping to api-recon phases

| api-recon | Parameter recon |
|---|---|
| Phase 1 Static | J1 anchor window expansion |
| Phase 2 A2 Interceptor | Globally injected fields (tenantId, sign) |
| Phase 3 runtime | J6 trigger matrix + `param_samples.json` |
| Phase 4 Permission tree | Different forms per module → all fields triggered only with sufficient permissions |
| Phase 5 Merge | `params_merged.json` + confidence; do not determine required from a single sample |

### J10. Troubleshooting

| Symptom | Handling |
|---|---|
| Static field name exists but runtime never appears | Mark as 'pending trigger'; supplement permission tree / click advanced filters / each option of linked select |
| Same path but different body shapes | Normal — record separately by `action`, do not force-merge schemas |
| Stub response is fake but you want to see params | **Look at the outbound request** body/headers, do not infer from the stub response |
| 400 reports nested field | Note the outer wrappers `data`/`bizData`/`variables` |
| GraphQL only shows operation name | Expand the `variables` JSON; statically find `$var: Type` |

---
