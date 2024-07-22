package processors

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/jmespath-community/go-jmespath/pkg/binding"
	"github.com/kyverno/chainsaw/pkg/apis/v1alpha1"
	"github.com/kyverno/chainsaw/pkg/client"
	"github.com/kyverno/chainsaw/pkg/discovery"
	"github.com/kyverno/chainsaw/pkg/model"
	"github.com/kyverno/chainsaw/pkg/report"
	apibindings "github.com/kyverno/chainsaw/pkg/runner/bindings"
	"github.com/kyverno/chainsaw/pkg/runner/cleanup"
	"github.com/kyverno/chainsaw/pkg/runner/clusters"
	"github.com/kyverno/chainsaw/pkg/runner/failer"
	"github.com/kyverno/chainsaw/pkg/runner/logging"
	"github.com/kyverno/chainsaw/pkg/runner/mutate"
	"github.com/kyverno/chainsaw/pkg/runner/names"
	"github.com/kyverno/chainsaw/pkg/runner/namespacer"
	"github.com/kyverno/chainsaw/pkg/runner/operations"
	opdelete "github.com/kyverno/chainsaw/pkg/runner/operations/delete"
	"github.com/kyverno/chainsaw/pkg/runner/summary"
	"github.com/kyverno/chainsaw/pkg/runner/timeout"
	"github.com/kyverno/chainsaw/pkg/testing"
	"github.com/kyverno/chainsaw/pkg/utils/kube"
	"github.com/kyverno/pkg/ext/output/color"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
)

type TestsProcessor interface {
	Run(context.Context, binding.Bindings)
	CreateTestProcessor(discovery.Test) TestProcessor
}

func NewTestsProcessor(
	config model.Configuration,
	clusters clusters.Registry,
	clock clock.PassiveClock,
	summary *summary.Summary,
	report *report.Report,
	tests ...discovery.Test,
) TestsProcessor {
	return &testsProcessor{
		config:   config,
		clusters: clusters,
		clock:    clock,
		summary:  summary,
		report:   report,
		tests:    tests,
	}
}

type testsProcessor struct {
	config   model.Configuration
	clusters clusters.Registry
	clock    clock.PassiveClock
	summary  *summary.Summary
	report   *report.Report
	tests    []discovery.Test
}

func (p *testsProcessor) Run(ctx context.Context, bindings binding.Bindings) {
	if bindings == nil {
		bindings = binding.NewBindings()
	}
	t := testing.FromContext(ctx)
	if p.report != nil {
		p.report.SetStartTime(time.Now())
		t.Cleanup(func() {
			p.report.SetEndTime(time.Now())
		})
	}
	var nspacer namespacer.Namespacer
	clusterConfig, clusterClient, err := p.clusters.Resolve(false)
	if err != nil {
		logging.Log(ctx, logging.Internal, logging.ErrorStatus, color.BoldRed, logging.ErrSection(err))
		failer.FailNow(ctx)
	}
	bindings = apibindings.RegisterClusterBindings(ctx, bindings, clusterConfig, clusterClient)
	if clusterClient != nil {
		if p.config.Namespace.Name != "" {
			namespace := kube.Namespace(p.config.Namespace.Name)
			object := kube.ToUnstructured(&namespace)
			bindings = apibindings.RegisterNamedBinding(ctx, bindings, "namespace", object.GetName())
			if p.config.Namespace.Template != nil && p.config.Namespace.Template.Value != nil {
				template := v1alpha1.Any{
					Value: p.config.Namespace.Template.Value,
				}
				if merged, err := mutate.Merge(ctx, object, bindings, template); err != nil {
					failer.FailNow(ctx)
				} else {
					object = merged
				}
				bindings = apibindings.RegisterNamedBinding(ctx, bindings, "namespace", object.GetName())
			}
			nspacer = namespacer.New(clusterClient, object.GetName())
			if err := clusterClient.Get(ctx, client.ObjectKey(&object), object.DeepCopy()); err != nil {
				if !errors.IsNotFound(err) {
					// Get doesn't log
					logging.Log(ctx, logging.Get, logging.ErrorStatus, color.BoldRed, logging.ErrSection(err))
					failer.FailNow(ctx)
				}
				if !cleanup.Skip(p.config.Cleanup.SkipDelete, nil, nil) {
					t.Cleanup(func() {
						operation := newOperation(
							OperationInfo{},
							false,
							timeout.Get(nil, p.config.Timeouts.CleanupDuration()),
							func(ctx context.Context, bindings binding.Bindings) (operations.Operation, binding.Bindings, error) {
								bindings = apibindings.RegisterClusterBindings(ctx, bindings, clusterConfig, clusterClient)
								return opdelete.New(clusterClient, object, nspacer, false, metav1.DeletePropagationBackground), bindings, nil
							},
							nil,
						)
						operation.execute(ctx, bindings)
					})
				}
				if err := clusterClient.Create(ctx, object.DeepCopy()); err != nil {
					failer.FailNow(ctx)
				}
			}
		}
	}
	shouldFailFast := &atomic.Bool{}
	for i := range p.tests {
		test := p.tests[i]
		name, err := names.Test(p.config, test)
		if err != nil {
			logging.Log(ctx, logging.Internal, logging.ErrorStatus, color.BoldRed, logging.ErrSection(err))
			failer.FailNow(ctx)
		}
		var scenarios []discovery.Test
		if test.Test != nil {
			if len(test.Test.Spec.Scenarios) == 0 {
				scenarios = append(scenarios, test)
			} else {
				for s := range test.Test.Spec.Scenarios {
					scenario := test.Test.Spec.Scenarios[s]
					test := test
					test.Test = test.Test.DeepCopy()
					test.Test.Spec.Scenarios = nil
					bindings := scenario.Bindings
					bindings = append(bindings, test.Test.Spec.Bindings...)
					test.Test.Spec.Bindings = bindings
					scenarios = append(scenarios, test)
				}
			}
		}
		for s := range scenarios {
			test := scenarios[s]
			t.Run(name, func(t *testing.T) {
				t.Helper()
				t.Cleanup(func() {
					if t.Failed() {
						shouldFailFast.Store(true)
					}
				})
				if p.summary != nil {
					t.Cleanup(func() {
						if t.Skipped() {
							p.summary.IncSkipped()
						} else {
							if t.Failed() {
								p.summary.IncFailed()
							} else {
								p.summary.IncPassed()
							}
						}
					})
				}
				if test.Test.Spec.Concurrent == nil || *test.Test.Spec.Concurrent {
					t.Parallel()
				}
				if test.Test.Spec.Skip != nil && *test.Test.Spec.Skip {
					t.SkipNow()
				}
				if p.config.Execution.FailFast {
					if shouldFailFast.Load() {
						t.SkipNow()
					}
				}
				processor := p.CreateTestProcessor(test)
				info := TestInfo{
					Id:         i + 1,
					ScenarioId: s + 1,
					Metadata:   test.Test.ObjectMeta,
				}
				processor.Run(
					testing.IntoContext(ctx, t),
					apibindings.RegisterNamedBinding(ctx, bindings, "test", info),
					nspacer,
				)
			})
		}
	}
}

func (p *testsProcessor) CreateTestProcessor(test discovery.Test) TestProcessor {
	var report *report.TestReport
	if p.report != nil {
		report = p.report.ForTest(&test)
	}
	return NewTestProcessor(p.config, p.clusters, p.clock, report, test)
}
