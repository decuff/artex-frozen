// Channel field table and config value parsing utilities.
//
// Split from the page because this is **data**, not a view: it describes which fields each channel has,
// which control each should use, and the bidirectional conversion between form text and config values (JSON).
// With it in a separate file, adding a channel only requires changes here; the page itself needs no edits.
// Display names and descriptions for channel types. Kept on the frontend because they only affect copy; the backend doesn't need to know.
export const KIND_LABEL: Record<string, string> = {
  dingtalk: "DingTalk",
  feishu: "Feishu",
  wecom: "WeCom",
  webhook: "Generic Webhook",
  telegram: "Telegram",
  email: "Email",
};

// Config field definitions for each channel.
//
// A frontend field table is deliberately kept here rather than having the backend deliver a schema: the backend only handles
// Validate (required/format), while the UI needs layout and control types — they're not the same concern.
// The only coupling point is secret_keys — which fields should render as password inputs is provided by the backend,
// because only the channel implementation itself knows which values count as credentials (the entire WeCom Webhook is a credential,
// whereas DingTalk's is just one secret). When adding a channel, a missing entry here only leaves the form blank,
// it won't fail silently (the hasFields below will flag it).
export type FieldKind = "text" | "password" | "number" | "select" | "textarea" | "switch" | "kv" | "list";
export interface FieldDef {
  key: string;
  label: string;
  kind: FieldKind;
  placeholder?: string;
  help?: string;
  options?: { value: string; label: string }[];
}
export const CHANNEL_FIELDS: Record<string, FieldDef[]> = {
  dingtalk: [
    {
      key: "webhook",
      label: "Webhook URL",
      kind: "text",
      placeholder: "https://oapi.dingtalk.com/robot/send?access_token=...",
    },
    {
      key: "secret",
      label: "Signing secret",
      kind: "password",
      help: "Fill in when the bot security setting is \"Sign\"; leave empty if \"Custom keyword\" is selected or security is not enabled",
    },
  ],
  feishu: [
    {
      key: "webhook",
      label: "Webhook URL",
      kind: "text",
      placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/...",
    },
    { key: "secret", label: "Signature verification secret", kind: "password", help: "Enter when the bot has \"signature verification\" enabled; otherwise leave blank" },
  ],
  wecom: [
    {
      key: "webhook",
      label: "Webhook URL",
      kind: "text",
      placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...",
    },
  ],
  webhook: [
    { key: "url", label: "Target URL", kind: "text", placeholder: "https://your-endpoint.example.com/hook" },
    {
      key: "method",
      label: "Request method",
      kind: "select",
      options: [
        { value: "POST", label: "POST (with request body)" },
        { value: "PUT", label: "PUT (with request body)" },
        { value: "PATCH", label: "PATCH (with request body)" },
        { value: "GET", label: "GET (no request body)" },
      ],
    },
    { key: "headers", label: "Custom headers", kind: "kv", help: "One KEY=VALUE per line, e.g. Authorization=Bearer xxx" },
    {
      key: "body_template",
      label: "Request body template",
      kind: "textarea",
      help:
        "Leave blank to use the built-in default template. Variables: {{.Title}} {{.Batch}} {{.Count}} {{.HomeURL}} {{.SentAt}}, " +
        "and .Name/.VulnClass/.Severity/.Summary/.Assets/.DetailURL/.StatusLabel under range .Items. " +
        "To insert strings use {{json .Xxx}} rather than {{.Xxx}}, or quotes in the title will break the JSON.",
    },
  ],
  telegram: [
    { key: "bot_token", label: "Bot Token", kind: "password", placeholder: "123456:ABC-DEF..." },
    { key: "chat_id", label: "Chat ID", kind: "text", placeholder: "-1001234567890" },
    {
      key: "base_url",
      label: "API address",
      kind: "text",
      placeholder: "https://api.telegram.org",
      help: "Leave blank to use the official address; fill in when using a self-hosted Bot API reverse proxy",
    },
  ],
  email: [
    { key: "host", label: "SMTP server", kind: "text", placeholder: "smtp.example.com" },
    {
      key: "port",
      label: "Port",
      kind: "number",
      placeholder: "587",
      help: "587 uses STARTTLS; for 465, enable \"implicit TLS\"",
    },
    { key: "username", label: "Account", kind: "text" },
    { key: "password", label: "Password / authorization code", kind: "password" },
    { key: "from", label: "Sender", kind: "text", placeholder: "artex@example.com" },
    { key: "to", label: "Recipients", kind: "list", help: "Separate multiple addresses with commas" },
    { key: "tls", label: "Implicit TLS", kind: "switch", help: "Enable for port 465; keep off for 587 (STARTTLS is used automatically)" },
  ],
};

export const SEVERITY_OPTIONS = [
  { value: "", label: "Any" },
  { value: "low", label: "Low and above" },
  { value: "medium", label: "Medium and above" },
  { value: "high", label: "High and above" },
  { value: "critical", label: "Critical only" },
];

export type ChannelForm = {
  name: string;
  kind: string;
  mode: "realtime" | "digest";
  enabled: boolean;
  ratePerMin: string;
  config: Record<string, unknown>;
  minSeverity: string;
  includeText: string;
  excludeText: string;
  taskIDsText: string;
  assetIDsText: string;
  onStatusChange: boolean;
};

export const emptyForm = (kind: string): ChannelForm => ({
  name: "",
  kind,
  mode: "realtime",
  enabled: true,
  ratePerMin: "",
  config: {},
  minSeverity: "",
  includeText: "",
  excludeText: "",
  taskIDsText: "",
  assetIDsText: "",
  onStatusChange: false,
});

// parseKV parses the "one KEY=VALUE per line" textarea.
export function parseKV(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    if (i > 0) out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return out;
}
// parseIDs parses a comma/whitespace-separated list of ids.
export function parseIDs(text: string): number[] {
  return text
    .split(/[\s,，]+/)
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => Number(s))
    .filter((n) => Number.isFinite(n) && n > 0);
}
// parseKeywords parses a line/comma-separated keyword list (vuln type names may contain spaces, so split by line or comma).
export function parseKeywords(text: string): string[] {
  return text
    .split(/[\n,，]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}
