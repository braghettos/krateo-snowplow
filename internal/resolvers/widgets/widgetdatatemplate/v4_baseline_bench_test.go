// v4_baseline_bench_test.go — Ship 0.30.240 v4 design Risk 12 baseline.
//
// PURPOSE. Capture the serve-time JQ re-evaluation cost on the LOAD-BEARING
// admin-compositions-list piechart widget shape, against 0.30.235 main code.
// The numbers anchor the v4 design's Risk 12 projection: can Tier 1-3
// mitigations (cohortGateMemo + CohortNSACL permitAll + per-cohort JQ memo)
// bring admin's serve-time cost under 100ms?
//
// SHAPE (production-realistic per feedback_no_fake_production_scenarios):
//   - DataSource simulates apistage RA output for compositions LIST:
//     { "list": { "items": [N×{namespace, name, status}], "metadata": {...} } }
//   - widgetDataTemplate items match the production composition-pie widget:
//     - "${ .list.items | length }"                       → total count
//     - "${ [.list.items[] | .status.phase] | unique }"   → phase series labels
//     - "${ [.list.items[] | .status.phase]
//          | group_by(.) | map({key:.[0], count:length})
//          | from_entries }"                              → phase histogram
//   - N ∈ {2, 100, 1000, 10000, 50000} — covers cyberjoker (~2),
//     typical (100-1000), admin-compositions (50K per project_argocd_apps_scale).
//
// SCOPE. NO production code change. NO commit. Benchmark + analysis only.
// Captures the WORK that v4 moves from resolve-time to serve-time on
// isRBACSensitiveApiRefWidget widgets.
//
// Run:
//   go test -run NONE -bench BenchmarkWidgetDataTemplate_V4Baseline \
//     -benchtime=2s -count=1 ./internal/resolvers/widgets/widgetdatatemplate/...
package widgetdatatemplate_test

import (
	"context"
	"fmt"
	"testing"

	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets/widgetdatatemplate"
)

// buildCompositionsListDS constructs a production-shape compositions LIST
// dataSource with N items. The "list" key matches a typical apistage RA
// output name in compositions-page widgets. Each item carries the fields
// the widget JQ touches: namespace, name, status.phase.
func buildCompositionsListDS(n int) map[string]any {
	phases := []string{"Ready", "Pending", "Failed", "Unknown"}
	items := make([]any, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, map[string]any{
			"metadata": map[string]any{
				"namespace": fmt.Sprintf("bench-ns-%02d", i%50),
				"name":      fmt.Sprintf("comp-%06d", i),
				"uid":       fmt.Sprintf("uid-%06d", i),
			},
			"status": map[string]any{
				"phase":   phases[i%len(phases)],
				"message": "ok",
			},
			"spec": map[string]any{
				"compositionDefinitionRef": map[string]any{
					"name":      "scaffolding",
					"namespace": "krateo-system",
				},
			},
		})
	}
	return map[string]any{
		"list": map[string]any{
			"apiVersion": "composition.krateo.io/v1-2-2",
			"kind":       "GithubScaffoldingList",
			"metadata":   map[string]any{"resourceVersion": "1"},
			"items":      items,
		},
	}
}

// compositionsPieTemplate mirrors a production piechart-over-compositions
// widget's widgetDataTemplate. Three expressions, increasing in cost:
//   - length  (cheap, single pass)
//   - unique  (single pass + dedup)
//   - group_by + map + from_entries  (the load-bearing aggregator)
func compositionsPieTemplate() []templatesv1.WidgetDataTemplate {
	return []templatesv1.WidgetDataTemplate{
		{
			ForPath:    "spec.widgetData.series.total",
			Expression: "${ .list.items | length }",
		},
		{
			ForPath:    "spec.widgetData.series.labels",
			Expression: "${ [.list.items[] | .status.phase] | unique }",
		},
		{
			ForPath:    "spec.widgetData.series.histogram",
			Expression: "${ [.list.items[] | .status.phase] | group_by(.) | map({key: .[0], count: length}) | from_entries }",
		},
	}
}

// BenchmarkWidgetDataTemplate_V4Baseline runs the full piechart JQ template
// over an N-item compositions list. This is what v4 RUNS AT SERVE TIME per
// /call when the widget is isRBACSensitiveApiRefWidget-classified (no Tier 1-3
// memos applied). Establishes the worst-case bound.
//
// Today (0.30.235 main): runs at RESOLVE time — once per cohort. v4 moves it
// to SERVE time — once per /call (Risk 12).
func BenchmarkWidgetDataTemplate_V4Baseline(b *testing.B) {
	sizes := []int{2, 100, 1000, 10000, 50000}
	tpl := compositionsPieTemplate()
	for _, n := range sizes {
		ds := buildCompositionsListDS(n)
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := widgetdatatemplate.Resolve(context.Background(),
					widgetdatatemplate.ResolveOptions{
						Items:      tpl,
						DataSource: ds,
					})
				if err != nil {
					b.Fatalf("Resolve: %v", err)
				}
			}
		})
	}
}

// BenchmarkWidgetDataTemplate_V4_CountOnly isolates the cheap aggregator
// (length only) — the cyberjoker-shape post-filter case where the narrowed
// row count is small but the user's piechart still needs to refresh.
//
// Cyberjoker scale (N=2): this is the lower bound serve-time cost when
// cohortGateMemo gives a small kept-set.
func BenchmarkWidgetDataTemplate_V4_CountOnly(b *testing.B) {
	sizes := []int{2, 100, 1000}
	tpl := []templatesv1.WidgetDataTemplate{
		{
			ForPath:    "spec.widgetData.series.total",
			Expression: "${ .list.items | length }",
		},
	}
	for _, n := range sizes {
		ds := buildCompositionsListDS(n)
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := widgetdatatemplate.Resolve(context.Background(),
					widgetdatatemplate.ResolveOptions{
						Items:      tpl,
						DataSource: ds,
					})
				if err != nil {
					b.Fatalf("Resolve: %v", err)
				}
			}
		})
	}
}

// compositionsPieTemplateMultiField mirrors a production widget that
// touches MULTIPLE item fields — closer in shape to a real datagrid or
// detailed-list widget than the single-field piechart fixture.
//
// Tightening 6 (peer-review): the single-field baseline (.status.phase only)
// may under-estimate production widget complexity. Real widgets often touch:
//   - metadata.name (display)
//   - metadata.namespace (grouping)
//   - spec.compositionDefinitionRef.name (filter)
//   - status.phase (status color)
//   - status.conditions (detail panel)
//
// 6 expressions across 4 distinct field paths exercises the gojq
// per-expression compile/parse overhead AND per-item field-access cost.
func compositionsPieTemplateMultiField() []templatesv1.WidgetDataTemplate {
	return []templatesv1.WidgetDataTemplate{
		// 1. Length (cheap, O(1) gojq fast-path).
		{
			ForPath:    "spec.widgetData.series.total",
			Expression: "${ .list.items | length }",
		},
		// 2. Unique namespaces (single pass + dedup).
		{
			ForPath:    "spec.widgetData.series.namespaces",
			Expression: "${ [.list.items[] | .metadata.namespace] | unique }",
		},
		// 3. Histogram by phase (group_by aggregator).
		{
			ForPath:    "spec.widgetData.series.phaseHistogram",
			Expression: "${ [.list.items[] | .status.phase] | group_by(.) | map({key: .[0], count: length}) | from_entries }",
		},
		// 4. Histogram by compositionDefinitionRef (second group_by).
		{
			ForPath:    "spec.widgetData.series.defHistogram",
			Expression: "${ [.list.items[] | .spec.compositionDefinitionRef.name] | group_by(.) | map({key: .[0], count: length}) | from_entries }",
		},
		// 5. Detail items projection (per-item map — full N scan).
		{
			ForPath:    "spec.widgetData.items",
			Expression: "${ [.list.items[] | {name: .metadata.name, namespace: .metadata.namespace, phase: .status.phase, def: .spec.compositionDefinitionRef.name}] }",
		},
		// 6. Conditions count (per-item field touch, single sum).
		{
			ForPath:    "spec.widgetData.series.totalConditions",
			Expression: "${ [.list.items[] | (.status.conditions // []) | length] | add // 0 }",
		},
	}
}

// BenchmarkWidgetDataTemplate_V4_MultiField — tightening 6.
// 6 JQ expressions across 4 distinct field paths. Captures the real
// production widget complexity that the single-field baseline elides.
//
// If multi-field is >2× single-field at any N, surface as a new risk and
// recalibrate Tier 1-3 sufficiency.
func BenchmarkWidgetDataTemplate_V4_MultiField(b *testing.B) {
	sizes := []int{2, 100, 1000, 10000, 50000}
	tpl := compositionsPieTemplateMultiField()
	for _, n := range sizes {
		ds := buildCompositionsListDS(n)
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := widgetdatatemplate.Resolve(context.Background(),
					widgetdatatemplate.ResolveOptions{
						Items:      tpl,
						DataSource: ds,
					})
				if err != nil {
					b.Fatalf("Resolve: %v", err)
				}
			}
		})
	}
}

// BenchmarkWidgetDataTemplate_V4_GroupByOnly isolates the load-bearing
// aggregator (group_by + map + from_entries) — the admin-scale case where
// the narrowed kept-set IS the full SA-maximal set (permitAll cohort).
//
// This is the load-bearing cost surfaced as Risk 12: if admin's serve-time
// group_by over 50K compositions exceeds 100ms, Tier 4 (lazy pagination at
// serve) mitigation is required.
func BenchmarkWidgetDataTemplate_V4_GroupByOnly(b *testing.B) {
	sizes := []int{1000, 10000, 50000}
	tpl := []templatesv1.WidgetDataTemplate{
		{
			ForPath:    "spec.widgetData.series.histogram",
			Expression: "${ [.list.items[] | .status.phase] | group_by(.) | map({key: .[0], count: length}) | from_entries }",
		},
	}
	for _, n := range sizes {
		ds := buildCompositionsListDS(n)
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := widgetdatatemplate.Resolve(context.Background(),
					widgetdatatemplate.ResolveOptions{
						Items:      tpl,
						DataSource: ds,
					})
				if err != nil {
					b.Fatalf("Resolve: %v", err)
				}
			}
		})
	}
}
