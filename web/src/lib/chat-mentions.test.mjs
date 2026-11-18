import { activeMention, mentionSearch, mentionToken, selectedMentions } from "./chat-mentions.ts";
import assert from "node:assert/strict";
import test from "node:test";

// Historical input and tokens must still decode to English display labels.
const legacyAsset = "\u8d44\u4ea7";
const legacyFinding = "\u6f0f\u6d1e";
const legacyQuery = "\u65e7\u8bb0\u5f55";

test("mention trigger supports Chinese and cursor placement without hijacking email", () => {
  assert.equal(activeMention("user@example.com", 16), null);
  assert.equal(activeMention("Selected @[Finding#1 X]", "Selected @[Fin".length), null);
  const prefix = "View ";
  const mention = "@Finding";
  const caret = prefix.length + mention.length;
  assert.deepEqual(activeMention(`${prefix}${mention} later`, caret), {
    start: prefix.length,
    end: caret,
    query: "Finding",
  });
  assert.equal(activeMention(`@${legacyAsset}`, 1 + legacyAsset.length)?.query, legacyAsset);
  assert.equal(activeMention("@Finding\nNext line", "@Finding\n".length), null);
});

test("categories, Chinese aliases, IP and keyword search", () => {
  assert.equal(mentionSearch("").categories.length, 9);
  assert.equal(mentionSearch("Fin").categories[0].kind, "finding");
  assert.equal(mentionSearch("Finding").kind, "finding");
  assert.equal(mentionSearch("Finding owner review").query, "owner review");
  assert.deepEqual(mentionSearch(`${legacyFinding}${legacyQuery}`), {
    kind: "finding",
    query: legacyQuery,
    categories: [],
  });
  assert.equal(mentionSearch("ip 192.0.2.1").kind, "ip");
  assert.equal(mentionSearch("Endpoint GET /api").query, "GET /api");
  assert.equal(mentionSearch("acme.com").kind, "");
});

test("tokens roundtrip labels and removing one reference preserves its neighbors", () => {
  const first = mentionToken({ kind: "finding", id: 12, label: "Title[1]\nDescription" });
  const second = mentionToken({ kind: "ip", id: 13, label: "192.0.2.1" });
  const value = `Analyze ${first} and ${second}`;
  const selected = selectedMentions(value);
  assert.equal(selected.length, 2);
  assert.equal(selected[0].label, "Finding #12 · Title(1) Description");
  assert.equal(selectedMentions(`@[${legacyFinding}#12 Original title]`)[0].label, "Finding #12 · Original title");
  assert.equal(selectedMentions("@[root_domain#8 Example]")[0].label, "Domain #8 · Example");
  assert.deepEqual(selectedMentions("@[Unknown#12]"), []);
  const next = value.slice(0, selected[0].start) + value.slice(selected[0].start + selected[0].token.length);
  assert.equal(selectedMentions(next)[0].token, second);
});
