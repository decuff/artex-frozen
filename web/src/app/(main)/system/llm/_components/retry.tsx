"use client";

// Shared pieces of LLM retry config: the 'count + interval' of each of the five retry layers.
//
// The five layers from inner to outer: connect (SDK) → empty response (SDK) → same-provider safety window → polling circuit breaker → intent rerun.
// The first three layers follow the endpoint, so each model config can override the global default; the last two are process-level, with only one global copy.
//
// All inputs follow the same 'blank = not configured' semantics, consistent with the backend db.RetryRule:
//   count   blank/0 = use built-in default | -1 = disable this retry layer | >0 = use this count
//   interval   blank/0 = use this layer's original exponential backoff | >0 = switch to this fixed millisecond interval

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from "@/lib/types";

export const ZERO_RULE: LLMRetryRule = { attempts: 0, interval_ms: 0 };
export const ZERO_OVERRIDE: LLMRetryOverride = {
  connect: ZERO_RULE,
  empty: ZERO_RULE,
  stream: ZERO_RULE,
};
const ZERO_POLICY: LLMRetryPolicy = {
  ...ZERO_OVERRIDE,
  breaker: ZERO_RULE,
  intent: ZERO_RULE,
};

type LayerMeta = {
  title: string;
  /** Where this retry layer happens and who executes it */
  where: string;
  /** What kind of error reaches this layer—down to the status code, don't make people guess */
  trigger: string;
  /** Errors that look like it but do [NOT] go through this layer, so people don't fill it in, see no effect, and think it's a bug */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** Default value when count is blank, used for placeholder */
  defAttempts: number;
  /** Default policy when interval is blank, used for placeholder */
  defInterval: string;
  /** Meaning of count set to -1 */
  offHint: string;
};

export const RETRY_LAYERS = {
  connect: {
    title: "Connect retry",
    where: "SDK · before getting 200",
    trigger:
      "Can't connect or haven't gotten 200 yet: connection reset / read-write timeout / DNS failure and other network-layer errors, plus HTTP 408, 429, 500, 502, 503, 504.",
    skips: "All other status codes (400 / 401 / 403 / 404 / 413 / 422 etc.) are deterministic rejections; resending fails the same way, so they are thrown up directly.",
    desc: "Resend the same request as-is. Once the stream starts (200 already received), a mid-stream disconnect is no longer this layer's concern.",
    attemptsLabel: "Retry count",
    defAttempts: 3,
    defInterval: "0.5s→1s→2s exponential (capped at 8s)",
    offHint: "-1 = don't retry at all, throw up immediately on failure",
  },
  empty: {
    title: "Empty response retry",
    where: "SDK · openai format only",
    trigger:
      "HTTP 200, finish_reason is a normal stop, but the entire response has not a single content block—gateway empty frames, dropped thinking-field frames, sampling hiccups all look like this.",
    skips: "No content due to max_tokens truncation doesn't count (that needs raising the output limit; resending just hits it again).",
    desc: "Resends the entire prompt, so it's relatively expensive on long contexts; the count shouldn't be set high.",
    attemptsLabel: "Retry count",
    defAttempts: 2,
    defInterval: "0.5s→1s→2s exponential (capped at 8s)",
    offHint: "-1 = empty response handed back as-is",
  },
  stream: {
    title: "Same-provider safe-window retry",
    where: "This project · before any output is delivered",
    trigger:
      "Problems that occur only after the stream is established (200 received): connection drops mid-stream, provider overloaded, in-stream 429 / 5xx error events — and not a single token has been handed to the caller yet.",
    skips:
      "Quota exhausted (402 / insufficient_quota, handled by rotation switching configs), context too long (413 / context length, handled by compaction), and deterministic rejections 400 / 401 / 403 / 404 / 422 are not retried.",
    desc: "Replays the same request on the same config. Since no output has been delivered yet, replaying won't duplicate model output or tool execution.",
    attemptsLabel: "Retry count",
    defAttempts: 2,
    defInterval: "0.5s→1s exponential (capped at 4s)",
    offHint: "-1 = stream break handed directly to the outer intent rerun",
  },
  breaker: {
    title: "Rotation circuit breaker",
    where: "This project · process-level, one global instance",
    trigger:
      "Trips when transient failures (429, 5xx, network errors) accumulate consecutively to the threshold; deterministic failures like insufficient balance (402), invalid key (401 / 403), or model not found (404) ignore the threshold and trip on the first occurrence.",
    skips: "One success resets the count to zero, so an occasionally flaky config won't slowly accumulate to a trip.",
    desc: "After tripping, it enters cooldown; during cooldown, rotation skips this config entirely. State is persisted, so it survives restarts.",
    attemptsLabel: "Consecutive failures before tripping",
    defAttempts: 3,
    defInterval: "1min→5min→30min gradient",
    offHint: "-1 = transient failures never trip (deterministic failures still trip)",
  },
  intent: {
    title: "Intent rerun",
    where: "This project · process-level, one global instance",
    trigger:
      "The earlier layers all failed to catch it: the worker ends with model_error — inner retries all exhausted, or the stream broke after output had already started being delivered (replay is unsafe then, so the whole thing must restart).",
    skips: "Quota exhaustion is already handled by rotation switching configs, so it isn't rerun here; when a task is paused / terminated / entering wrap-up, it yields immediately without consuming backoff time.",
    desc: "Runs the entire intent from scratch again. It's the outermost layer, so one rerun means the inner layers' counts multiply again.",
    attemptsLabel: "Rerun count",
    defAttempts: 2,
    defInterval: "Fixed 3s",
    offHint: "-1 = no rerun, this intent is directly judged as blocked",
  },
} satisfies Record<string, LayerMeta>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** Human-readable milliseconds, only used for echoing next to the input box, to avoid counting zeros. */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** Controlled number input: empty string ↔ 0, intermediate states ("-", "1e") stay locally as-is, without disturbing the parent. */
function NumField({
  id,
  value,
  onChange,
  placeholder,
  min,
}: {
  id: string;
  value: number;
  onChange: (n: number) => void;
  placeholder: string;
  min: number;
}) {
  const [text, setText] = React.useState(value === 0 ? "" : String(value));
  // Follow when the parent swaps in a whole new set of values (loaded policy, switched config); won't reach here when typing yourself,
  // because at that point value already equals the parsed result of the local text.
  React.useEffect(() => {
    const incoming = value === 0 ? "" : String(value);
    setText((cur) => (Number(cur || 0) === value ? cur : incoming));
  }, [value]);
  return (
    <Input
      id={id}
      type="number"
      min={min}
      className="w-28 shrink-0"
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const n = Number(e.target.value);
        onChange(e.target.value.trim() === "" || !Number.isFinite(n) ? 0 : Math.trunc(n));
      }}
    />
  );
}

/** The two knobs for one retry layer. idPrefix is used to preserve the label's htmlFor when appearing multiple times on the same page. */
export function RetryRuleFields({
  layer,
  idPrefix,
  value,
  onChange,
  compact,
}: {
  layer: LayerKey;
  idPrefix: string;
  value: LLMRetryRule;
  onChange: (r: LLMRetryRule) => void;
  /** true = compact version in the config drawer: omit the expanded description, keep only the line 'which errors reach this layer' */
  compact?: boolean;
}) {
  const meta = RETRY_LAYERS[layer];
  const human = humanMs(value.interval_ms);
  return (
    <div className={compact ? "grid gap-2" : "grid gap-3 rounded-lg border p-3"}>
      <div className="grid gap-0.5">
        <div className="flex flex-wrap items-baseline gap-2">
          <Label className="text-sm">{meta.title}</Label>
          <span className="text-muted-foreground text-xs">{meta.where}</span>
        </div>
        {/* Which errors reach this layer, down to the status code—if you filled in the knobs but see no effect, it's likely the error doesn't land on this layer at all. */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">Trigger</span>: {meta.trigger}
        </p>
        {!compact && meta.skips && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">Skips this layer</span>: {meta.skips}
          </p>
        )}
        {!compact && <p className="text-muted-foreground text-xs">{meta.desc}</p>}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-n`} className="text-muted-foreground text-xs">
            {meta.attemptsLabel}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-n`}
            min={-1}
            value={value.attempts}
            placeholder={`Default ${meta.defAttempts}`}
            onChange={(n) => onChange({ ...value, attempts: n })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-ms`} className="text-muted-foreground text-xs">
            Interval ms
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder="Default backoff"
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">{human ? `Fixed ${human}` : meta.defInterval}</span>
        </div>
      </div>
      {!compact && <p className="text-muted-foreground text-xs">Leave empty = use default; {meta.offHint}.</p>}
    </div>
  );
}

/** The three layers of override in the model config drawer (the three layers that follow the endpoint). */
export function ProfileRetryFields({
  value,
  onChange,
}: {
  value: LLMRetryOverride;
  onChange: (o: LLMRetryOverride) => void;
}) {
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="grid gap-0.5">
        <Label className="text-sm">Retry overrides</Label>
        <p className="text-muted-foreground text-xs">
          Only applies to this config, overriding the global defaults in 'Retry & Backoff'. Leave each field empty = follow global; attempts -1 = disable this retry layer;
          if interval is filled, a fixed interval replaces exponential backoff. Circuit breaker and intent rerun are process-level, only adjustable on the global page.
        </p>
      </div>
      {(["connect", "empty", "stream"] as const).map((k) => (
        <div key={k} className="border-t pt-3 first:border-t-0 first:pt-0">
          <RetryRuleFields
            compact
            layer={k}
            idPrefix="pf"
            value={value[k]}
            onChange={(r) => onChange({ ...value, [k]: r })}
          />
        </div>
      ))}
    </div>
  );
}

/** 'Retry & Backoff' tab: global defaults for the five layers. */
export function RetryPolicyPanel() {
  const [policy, setPolicy] = React.useState<LLMRetryPolicy>(ZERO_POLICY);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const p = await api.llmRetryPolicy();
      setPolicy({ ...ZERO_POLICY, ...p });
    } catch (e) {
      toast.error(`Failed to read retry policy: ${(e as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      // The backend clamps out-of-range values back into the range and returns them; refresh directly with the returned value, what you see is what's stored.
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success("Saved, effective immediately (the currently running round of calls still uses the old parameters)");
    } catch (e) {
      toast.error(`Save failed: ${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  const set = (k: LayerKey) => (r: LLMRetryRule) => setPolicy((p) => ({ ...p, [k]: r }));

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
        <Loader2Icon className="size-4 animate-spin" /> Reading retry policy…
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        A single model call failure passes through five retry layers in sequence, from innermost to outermost:
        <span className="text-foreground"> connection setup → empty response → same-provider safety window → round-robin circuit breaker → intent re-run</span>
        . The outer layer only kicks in after the inner one is exhausted, so the counts
        <span className="text-foreground">multiply</span>
        —maxing out every layer means a single hiccup can burn through dozens of requests.
        Leave everything blank to use the current defaults, exactly matching behavior when this page doesn't exist. The first three layers can be overridden individually in each model's config.
      </div>

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(Object.keys(RETRY_LAYERS) as LayerKey[]).map((k) => (
          <RetryRuleFields key={k} layer={k} idPrefix="gl" value={policy[k]} onChange={set(k)} />
        ))}
      </div>

      <div className="flex gap-2">
        <Button onClick={save} disabled={saving}>
          {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />}
          Save
        </Button>
        <Button variant="outline" onClick={() => setPolicy(ZERO_POLICY)} disabled={saving}>
          Restore all defaults
        </Button>
      </div>
    </div>
  );
}
