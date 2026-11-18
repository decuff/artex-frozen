package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// Server-side parsing of the retry policy, see docs/LLM retry design.md. Among the five layers:
//   - Connection setup / empty response / same-provider safety window are 'per-endpoint', each LLM config can override
//     the global default (if a profile field is left empty it inherits the global, and if the global is also unset the built-in default is used);
//   - Circuit breaker / intent re-run are process-level, only one global copy.
//
// The global policy reads one DB row of settings once; call sites are all on low-frequency paths (building a provider, work finalization,
// saving config), not worth adding another cache layer; circuit breaker params are the exception — they are read on every failure path, so
// they are pushed by applyRetryPolicy to the Registry for storage.

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// The count keeps the original semantics of '0=default / negative=off' here: the SDK's MaxRetries /
		// EmptyResponseRetries are fully isomorphic to it, just let it parse them itself.
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// Default values for the circuit breaker (polling cooldown), consistent with llmpool's built-in — here it only overrides when 'the user has configured a value'.
// Default values for intent re-run see modelErrorRetries / modelErrorRetryBackoff in engine.go.

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// inject (see steerHooks.Stop). It deliberately reuses layer ②'s knob — 'empty response
// retry count': the two are two means to the same end. The SDK layer handles 'not a single content block', by resending
// the same request as-is; here it handles 'only thinking, neither body text nor tools', by appending an instruction to let
// the model continue with its existing thinking (resending as-is is meaningless for this kind of idle spin determined by context shape). The emptiness criteria
// differ because the SDK goes by 'whether any event was yielded', and thinking deltas are themselves events — but when the user configures
// 'how many times to retry on empty response' they mean 'if the model produced no substantive content, try again', and the two layers sharing one count
// matches that mental model.
//
// Read the global policy rather than some profile's override: a run may switch profiles mid-way due to failover, and this
// is the total volume gate for the whole intent, it should not change with the endpoint. Semantics are isomorphic to the SDK's emptyRetries():
// 0 = default defaultEmptyTurnNudges; -1 (negative) = disable idle continuation; >0 = use that value.
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
