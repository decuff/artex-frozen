package db

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Matching/execution layer for asset intercept rules. asset_intercept.go only handles rule storage; this handles matching
// the domain/IP/URL of the 'target asset' against enabled rules. Used by agent tools (add_intent,
// insert_assets) before dispatching intents / inserting assets; if a match is hit, reject.

// AssetInterceptKindLabel returns the Chinese label for kind, used for explanation messages to the agent.
func AssetInterceptKindLabel(kind string) string {
	switch kind {
	case "exact_domain":
		return "Domain(exact)"
	case "exact_ip":
		return "IP(exact)"
	case "exact_url":
		return "URL(exact)"
	case "fuzzy_domain":
		return "Domain(fuzzy)"
	case "fuzzy_ip":
		return "IP(fuzzy)"
	case "fuzzy_url":
		return "URL(fuzzy)"
	case "cidr":
		return "CIDR subnet"
	}
	return kind
}

// Reason returns a readable hit reason, in the form: hit asset intercept rule [Domain(fuzzy): .gov.cn] (note).
func (r AssetInterceptRule) Reason() string {
	s := fmt.Sprintf("hit asset intercept rule [%s: %s]", AssetInterceptKindLabel(r.Kind), r.Pattern)
	if note := strings.TrimSpace(r.Note); note != "" {
		s += "（" + note + "）"
	}
	return s
}

// matchOne determines whether a single enabled rule matches the given domain/IP/URL candidate string, returning the specific matched value.
func matchOne(r AssetInterceptRule, domains, ips, urls []string) (string, bool) {
	p := strings.TrimSpace(r.Pattern)
	if p == "" {
		return "", false
	}
	switch r.Kind {
	case "exact_domain":
		for _, d := range domains {
			if strings.EqualFold(strings.TrimSpace(d), p) {
				return d, true
			}
		}
	case "exact_ip":
		for _, ip := range ips {
			if strings.TrimSpace(ip) == p {
				return ip, true
			}
		}
	case "exact_url":
		for _, u := range urls {
			if strings.TrimSpace(u) == p {
				return u, true
			}
		}
	case "fuzzy_domain":
		lp := strings.ToLower(p)
		for _, d := range domains {
			if d != "" && strings.Contains(strings.ToLower(d), lp) {
				return d, true
			}
		}
	case "fuzzy_ip":
		for _, ip := range ips {
			if ip != "" && strings.Contains(ip, p) {
				return ip, true
			}
		}
	case "fuzzy_url":
		lp := strings.ToLower(p)
		for _, u := range urls {
			if u != "" && strings.Contains(strings.ToLower(u), lp) {
				return u, true
			}
		}
	case "cidr":
		_, ipnet, err := net.ParseCIDR(p)
		if err != nil {
			return "", false
		}
		for _, ip := range ips {
			if pip := net.ParseIP(strings.TrimSpace(ip)); pip != nil && ipnet.Contains(pip) {
				return ip, true
			}
		}
	}
	return "", false
}

// MatchAssetInterceptRules returns the first enabled rule that matches the given domain/IP/URL candidate string, along with the specific matched value.
// Used by insert_assets to match against raw input (assetInputItem not yet persisted).
func MatchAssetInterceptRules(rules []AssetInterceptRule, domains, ips, urls []string) (AssetInterceptRule, string, bool) {
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if v, ok := matchOne(r, domains, ips, urls); ok {
			return r, v, true
		}
	}
	return AssetInterceptRule{}, "", false
}

// interceptCandidates extracts the domain/IP/URL candidate strings of a persisted asset for intercept matching.
// The host of a URL is split out and categorized, so that service-type assets that 'only carry a URL' can also be matched by domain/IP rules.
func (a *Asset) interceptCandidates() (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, a.Domain)
	add(&domains, a.RootDomain)
	for _, d := range a.BoundDomains {
		add(&domains, d)
	}
	add(&ips, a.IP)
	add(&urls, a.URL)
	if a.URL != "" {
		if u, err := url.Parse(a.URL); err == nil {
			if h := u.Hostname(); h != "" {
				if net.ParseIP(h) != nil {
					add(&ips, h)
				} else {
					add(&domains, h)
				}
			}
		}
	}
	return domains, ips, urls
}

// InterceptLabel returns a short identifier for the asset, used for explanation messages to the agent.
func (a *Asset) InterceptLabel() string {
	var target string
	switch {
	case a.Domain != "":
		target = a.Domain
	case a.URL != "":
		target = a.URL
	case a.IP != "":
		target = a.IP
	default:
		target = fmt.Sprintf("#%d", a.ID)
	}
	return fmt.Sprintf("asset#%d[%s] %s", a.ID, a.Type, target)
}

// hasEnabledRule determines whether any enabled rule exists in the rule set.
func hasEnabledRule(rules []AssetInterceptRule) bool {
	for _, r := range rules {
		if r.Enabled {
			return true
		}
	}
	return false
}

// AssetGateDecision is the verdict of the 'intercept first, then allow' gate for a set of candidate strings.
type AssetGateDecision struct {
	Allowed bool
	Reason  string // Rejection reason (excluding asset identifier); empty when Allowed=true
}

// EvaluateAssetGate performs the task-level gate verdict:
//  1. If any enabled blockRules match → reject (intercept reason).
//  2. Otherwise, if allowRules has enabled entries and none match → deny (not in the allowed range).
//  3. Otherwise allow.
//
// When allowRules is empty/has no enabled entries, the allow gate does not take effect (i.e., the whitelist is not enabled, everything is allowed),
// to avoid 'no allow rules configured' blocking all assets.
func EvaluateAssetGate(blockRules, allowRules []AssetInterceptRule, domains, ips, urls []string) AssetGateDecision {
	if rule, _, ok := MatchAssetInterceptRules(blockRules, domains, ips, urls); ok {
		return AssetGateDecision{Allowed: false, Reason: rule.Reason()}
	}
	if hasEnabledRule(allowRules) {
		if _, _, ok := MatchAssetInterceptRules(allowRules, domains, ips, urls); !ok {
			return AssetGateDecision{Allowed: false, Reason: "Not within the task's allowed (whitelist) range, testing not permitted"}
		}
	}
	return AssetGateDecision{Allowed: true}
}

// AssetInterceptHit describes an asset rejected by the gate (intercept hit or not in the allowed range).
type AssetInterceptHit struct {
	Asset  *Asset
	Reason string // readable reason
}

// Describe returns a readable explanation: asset info + reason.
func (h AssetInterceptHit) Describe() string {
	return fmt.Sprintf("%s → %s", h.Asset.InterceptLabel(), h.Reason)
}

// ListAssetInterceptRules is a passthrough of the *DB method of the same name, so callers that only hold an AssetStore
// (such as agent tools) can also read the rules.
func (s *AssetStore) ListAssetInterceptRules() ([]AssetInterceptRule, error) {
	return s.db.ListAssetInterceptRules()
}

// CheckAssetsIntercept loads assets by id, runs the 'intercept first, then allow' gate decision on each, and returns all
// rejected assets. Intercept rules = global ∪ task-level block; allow rules = task-level allow (this task only).
// Fast return when there are no ids. Uses the global GetByIDs (not filtered by task scope) to ensure interception is not weakened by scope.
func (s *AssetStore) CheckAssetsIntercept(taskID int64, ids []int64) ([]AssetInterceptHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	blockRules, err := s.db.ListAssetInterceptRules()
	if err != nil {
		return nil, err
	}
	var allowRules []AssetInterceptRule
	if taskID > 0 {
		tb, ta, err := s.TaskInterceptRulesSplit(taskID)
		if err != nil {
			return nil, err
		}
		blockRules = append(blockRules, tb...)
		allowRules = ta
	}
	// Neither intercept rules nor enabled allow rules → no decision needed, all allowed.
	if len(blockRules) == 0 && !hasEnabledRule(allowRules) {
		return nil, nil
	}
	assets, err := s.GetByIDs(ids)
	if err != nil {
		return nil, err
	}
	var hits []AssetInterceptHit
	for _, a := range assets {
		domains, ips, urls := a.interceptCandidates()
		if d := EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
			hits = append(hits, AssetInterceptHit{Asset: a, Reason: d.Reason})
		}
	}
	return hits, nil
}
