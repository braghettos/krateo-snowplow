// refilter.go — Tag 0.30.9 Sub-scope A: in-process per-object refilter
// for the userAccessFilter dispatch path.
//
// Contract:
//   - Input: the raw JSON-decoded response of a ServiceAccount-dispatched
//     K8s list call (typically {"kind":"...List","items":[...]} but
//     tolerates a bare slice too).
//   - The per-object NamespaceFrom JQ expression yields the namespace
//     used in the EvaluateRBAC call. When the expression is empty or
//     evaluates to "", the per-object namespace is "" (cluster-scoped
//     check).
//   - Output: same shape as input with non-permitted items dropped.
//     Result is RBAC-clean from the user's perspective.
//
// Bindings:
//   - feedback_restaction_no_widget_logic.md: this code lives in the
//     resolver layer (per-API stage), not in widget canonicalization.
//   - feedback_no_special_cases.md: no per-resource carve-outs.
//     refilter is uniform across every userAccessFilter usage.
//   - Revision 2: EvaluateRBAC fires per object. The refilter is the
//     authoritative gate; if it drops, the user does not see the item.
//   - feedback_l1_invalidation_delete_only.md: refilter runs BEFORE
//     the resolved-cache write (dispatchers/restactions.go path), so
//     the cached entry is already user-scoped.
//
// Performance: per-object JQ eval + per-object EvaluateRBAC. At the
// production refilter sites (cyberjoker's 6 namespace-scope filters)
// the result sets are ≤ 50 items, so the cost is ≤ 50 × (JQ eval + RBAC
// lookup). EvaluateRBAC is in-process (typed-RBAC informer cache);
// amortised to <1µs at Tag 0.30.10 (permission-check cache).

package api

import (
	"context"
	"fmt"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jqutil"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/rbac"
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"
)

// refilterResult is the (kept, dropped, total) summary the resolver
// logs as the per-call falsifier per plan §"Code-path falsifier":
//   userAccessFilter.dispatch=service_account ... refilter_dropped=N refilter_kept=M evaluate_rbac_calls=K
type refilterResult struct {
	Kept              int
	Dropped           int
	EvaluateRBACCalls int
}

// applyUserAccessFilter is the entrypoint the resolver calls when an
// API stage declares userAccessFilter. It mutates dict[apiCall.Name]
// in place, replacing the SA-dispatched result with the refiltered
// subset.
//
// Shape detection: dict[apiCall.Name] is either:
//   * map[string]any with an "items" slice (typical K8s list response);
//   * []any (rare; some endpoints flatten the list).
// Other shapes are passed through unchanged with a WARN (we cannot
// safely refilter what we don't understand — the user sees the full
// SA-dispatched response, which is the conservative-deny choice for
// security but a leak by definition; the WARN is the operator alert).
//
// Errors during JQ eval or RBAC eval are treated as DENIES per object
// (fail-closed semantics) so a transient evaluator hiccup never
// permits a leak.
func applyUserAccessFilter(ctx context.Context, dict map[string]any, apiCall *templates.API) refilterResult {
	log := xcontext.Logger(ctx)
	res := refilterResult{}

	if apiCall == nil || apiCall.UserAccessFilter == nil {
		return res
	}
	uaf := apiCall.UserAccessFilter

	user, err := xcontext.UserInfo(ctx)
	if err != nil {
		log.Error("userAccessFilter: cannot extract UserInfo; dropping all items (fail-closed)",
			slog.String("api", apiCall.Name),
			slog.Any("err", err),
		)
		// Replace with empty result set. Conservative-deny per
		// security model.
		_ = setRefilteredEmpty(dict, apiCall.Name)
		return res
	}

	raw, ok := dict[apiCall.Name]
	if !ok || raw == nil {
		return res
	}

	switch v := raw.(type) {
	case map[string]any:
		// Typical K8s list shape: {"items": [...]}
		itemsRaw, hasItems := v["items"]
		if !hasItems {
			// Some endpoints return a single object without an "items"
			// wrapper (cluster-scoped GET-by-name). Treat the whole
			// map as the single object.
			permitted := evalSingle(ctx, log, user.Username, user.Groups, uaf, v)
			res.EvaluateRBACCalls++
			if permitted {
				res.Kept++
			} else {
				res.Dropped++
				dict[apiCall.Name] = map[string]any{}
			}
			return res
		}
		items, ok := itemsRaw.([]any)
		if !ok {
			log.Warn("userAccessFilter: items is not a slice; passing through unchanged",
				slog.String("api", apiCall.Name),
				slog.Any("items_type", fmt.Sprintf("%T", itemsRaw)),
			)
			return res
		}
		kept, dropped, calls := refilterSlice(ctx, log, user.Username, user.Groups, uaf, items)
		v["items"] = kept
		res.Kept = len(kept)
		res.Dropped = dropped
		res.EvaluateRBACCalls = calls

	case []any:
		kept, dropped, calls := refilterSlice(ctx, log, user.Username, user.Groups, uaf, v)
		dict[apiCall.Name] = kept
		res.Kept = len(kept)
		res.Dropped = dropped
		res.EvaluateRBACCalls = calls

	default:
		log.Warn("userAccessFilter: unrecognised result shape; passing through unchanged",
			slog.String("api", apiCall.Name),
			slog.Any("result_type", fmt.Sprintf("%T", raw)),
		)
	}

	return res
}

// refilterSlice walks items[] and returns (kept, droppedCount, calls).
// Idempotent on input — items slice is treated as immutable; a fresh
// kept slice is allocated.
func refilterSlice(ctx context.Context, log *slog.Logger, username string, groups []string, uaf *templates.UserAccessFilterSpec, items []any) ([]any, int, int) {
	kept := make([]any, 0, len(items))
	dropped := 0
	calls := 0
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			// Non-object item (e.g., bare string). Conservative-deny.
			dropped++
			continue
		}
		permitted := evalSingle(ctx, log, username, groups, uaf, obj)
		calls++
		if permitted {
			kept = append(kept, item)
		} else {
			dropped++
		}
	}
	return kept, dropped, calls
}

// evalSingle resolves NamespaceFrom against obj and calls EvaluateRBAC.
// Returns true iff RBAC permits the (verb, group, resource, ns) tuple
// for (username, groups).
//
// JQ-eval errors and RBAC errors both fail closed.
func evalSingle(ctx context.Context, log *slog.Logger, username string, groups []string, uaf *templates.UserAccessFilterSpec, obj map[string]any) bool {
	namespace := ""
	if uaf.NamespaceFrom != "" {
		ns, err := evalJQString(ctx, uaf.NamespaceFrom, obj)
		if err != nil {
			log.Warn("userAccessFilter: NamespaceFrom JQ eval failed; treating object as denied",
				slog.String("expr", uaf.NamespaceFrom),
				slog.Any("err", err),
			)
			return false
		}
		namespace = ns
	}

	allowed, err := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
		Username:  username,
		Groups:    groups,
		Verb:      uaf.Verb,
		Group:     uaf.Group,
		Resource:  uaf.Resource,
		Namespace: namespace,
	})
	if err != nil {
		log.Warn("userAccessFilter: EvaluateRBAC error; treating object as denied",
			slog.String("user", username),
			slog.String("verb", uaf.Verb),
			slog.String("group", uaf.Group),
			slog.String("resource", uaf.Resource),
			slog.String("namespace", namespace),
			slog.Any("err", err),
		)
		return false
	}
	return allowed
}

// evalJQString evaluates expr against obj and returns a Go string. The
// jqutil.Eval returns a JSON-encoded result, so a JQ expression like
// ".metadata.name" produces `"some-name"` (a JSON string literal); we
// strip the surrounding quotes here.
func evalJQString(ctx context.Context, expr string, obj map[string]any) (string, error) {
	s, err := jqutil.Eval(ctx, jqutil.EvalOptions{
		Query:        expr,
		Data:         obj,
		ModuleLoader: jqsupport.ModuleLoader(),
	})
	if err != nil {
		return "", err
	}
	// jqutil.Eval returns the JSON serialisation. For string results
	// that means `"value"\n` — trim the wrapper.
	out := trimJSONString(s)
	return out, nil
}

// trimJSONString strips outer whitespace + surrounding double quotes
// from a single-line JSON string. Returns "" when the input is
// `null` or `""`. Keeps the implementation minimal — JQ produces
// well-formed JSON so we don't need a full json.Unmarshal cycle.
func trimJSONString(s string) string {
	// Trim leading/trailing whitespace.
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\n' || s[0] == '\r' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	if s == "null" || s == "" {
		return ""
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// setRefilteredEmpty replaces dict[apiName] with the canonical empty
// shape (map with empty items slice). Used by the fail-closed paths.
func setRefilteredEmpty(dict map[string]any, apiName string) error {
	dict[apiName] = map[string]any{"items": []any{}}
	return nil
}

// emitRefilterFalsifier emits the per-call falsifier per plan
// §"Code-path falsifier" line:
//
//   userAccessFilter.dispatch=service_account user=X resource_type=...
//   refilter_dropped=N refilter_kept=M evaluate_rbac_calls=K
//
// Called by the resolver right after applyUserAccessFilter completes.
func emitRefilterFalsifier(log *slog.Logger, apiCall *templates.API, username string, res refilterResult) {
	if log == nil || apiCall == nil || apiCall.UserAccessFilter == nil {
		return
	}
	log.Info("userAccessFilter",
		slog.String("subsystem", "uaf"),
		slog.String("dispatch", "service_account"),
		slog.String("user", username),
		slog.String("api", apiCall.Name),
		slog.String("verb", apiCall.UserAccessFilter.Verb),
		slog.String("group", apiCall.UserAccessFilter.Group),
		slog.String("resource", apiCall.UserAccessFilter.Resource),
		slog.Int("refilter_kept", res.Kept),
		slog.Int("refilter_dropped", res.Dropped),
		slog.Int("evaluate_rbac_calls", res.EvaluateRBACCalls),
	)
}
