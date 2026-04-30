// Prometheus metrics. Opt-in via Agent.Metrics; nil is a no-op so the
// no-metrics path has no overhead. Cardinality is bounded by the
// caller's static config (closed-enum labels plus model/tool names
// from the registry); the agent never mints labels per-request.

package starling

import (
	"net/http"
	"time"

	"github.com/jerkeyray/starling/step"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every collector Starling exposes. Construct via
// NewMetrics against a Registerer the operator owns; assign the
// result to Agent.Metrics. The zero value is not usable — nil is
// the "disabled" sentinel.
type Metrics struct {
	runsStarted  prometheus.Counter
	runsInFlight prometheus.Gauge
	runTerminal  *prometheus.CounterVec   // labels: status, error_type
	runDuration  *prometheus.HistogramVec // labels: status

	providerCalls    *prometheus.CounterVec   // labels: model, status
	providerDuration *prometheus.HistogramVec // labels: model
	providerTokens   *prometheus.CounterVec   // labels: model, type

	toolCalls    *prometheus.CounterVec   // labels: tool, status, error_type
	toolDuration *prometheus.HistogramVec // labels: tool

	eventlogAppends  *prometheus.CounterVec   // labels: kind, status
	eventlogDuration *prometheus.HistogramVec // labels: kind

	budgetExceeded *prometheus.CounterVec // labels: axis
}

// NewMetrics registers every collector against reg and returns a
// Metrics ready to attach to an Agent. Panics on duplicate
// registration — same posture as BearerAuth("") — so misuse
// surfaces at startup rather than silently discarding samples.
//
// Pass a fresh prometheus.NewRegistry() per process to avoid
// accidentally colliding with globally-registered collectors.
// promhttp.Handler(reg) then exposes the scrape endpoint.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		panic("starling: NewMetrics called with nil Registerer")
	}

	// Exponential bucket span chosen once: 5ms→120s covers agent runs,
	// LLM streams, and tool latency without splitting the distribution
	// badly. 12 buckets keeps the series count reasonable. The eventlog
	// histogram uses a tighter band because that's where we want tail
	// spikes (disk trouble) to be visible.
	runBuckets := prometheus.ExponentialBucketsRange(0.005, 120, 12)
	appendBuckets := prometheus.ExponentialBucketsRange(0.00005, 0.1, 10)

	m := &Metrics{
		runsStarted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "starling_runs_started_total",
			Help: "Total number of agent runs started.",
		}),
		runsInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "starling_runs_in_flight",
			Help: "Number of agent runs currently executing.",
		}),
		runTerminal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "starling_run_terminal_total",
			Help: "Agent runs by terminal status. status={ok,error,cancelled,budget}; error_type is 'none' on ok/cancelled.",
		}, []string{"status", "error_type"}),
		runDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "starling_run_duration_seconds",
			Help:    "Wall-clock duration of agent runs, labelled by terminal status.",
			Buckets: runBuckets,
		}, []string{"status"}),

		providerCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "starling_provider_calls_total",
			Help: "Provider Stream() calls. status={ok,error,cancelled}.",
		}, []string{"model", "status"}),
		providerDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "starling_provider_call_duration_seconds",
			Help:    "Wall-clock duration of a provider streaming call, measured from Stream() open to EOF or error.",
			Buckets: runBuckets,
		}, []string{"model"}),
		providerTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "starling_provider_tokens_total",
			Help: "Tokens reported by the provider per call. type={prompt,completion}.",
		}, []string{"model", "type"}),

		toolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "starling_tool_calls_total",
			Help: "Tool invocations. status={ok,error}; error_type is 'none' on ok, else {tool,panic,cancelled,other}.",
		}, []string{"tool", "status", "error_type"}),
		toolDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "starling_tool_call_duration_seconds",
			Help:    "Wall-clock duration of a single tool invocation attempt.",
			Buckets: runBuckets,
		}, []string{"tool"}),

		eventlogAppends: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "starling_eventlog_appends_total",
			Help: "Event-log append calls. status={ok,error}; kind is event.Kind.String().",
		}, []string{"kind", "status"}),
		eventlogDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "starling_eventlog_append_duration_seconds",
			Help:    "Wall-clock duration of a single event-log append.",
			Buckets: appendBuckets,
		}, []string{"kind"}),

		budgetExceeded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "starling_budget_exceeded_total",
			Help: "Budget trips by axis. axis={input_tokens,output_tokens,cost_usd,wall_clock}.",
		}, []string{"axis"}),
	}

	reg.MustRegister(
		m.runsStarted, m.runsInFlight, m.runTerminal, m.runDuration,
		m.providerCalls, m.providerDuration, m.providerTokens,
		m.toolCalls, m.toolDuration,
		m.eventlogAppends, m.eventlogDuration,
		m.budgetExceeded,
	)
	return m
}

// MetricsHandler is a convenience wrapper so users don't have to
// import promhttp themselves for the common case. Equivalent to
// promhttp.HandlerFor(g, promhttp.HandlerOpts{}).
func MetricsHandler(g prometheus.Gatherer) http.Handler {
	if g == nil {
		panic("starling: MetricsHandler called with nil Gatherer")
	}
	return promhttp.HandlerFor(g, promhttp.HandlerOpts{})
}

// Record methods are all nil-safe; callers invoke them unconditionally.

// onRunStarted bumps runs_started and runs_in_flight. Call once per
// Run entry.
func (m *Metrics) onRunStarted() {
	if m == nil {
		return
	}
	m.runsStarted.Inc()
	m.runsInFlight.Inc()
}

// onRunFinished decrements the in-flight gauge. Pair with
// onRunStarted in a defer so panics don't leak gauge count.
func (m *Metrics) onRunFinished() {
	if m == nil {
		return
	}
	m.runsInFlight.Dec()
}

// onRunTerminal records the terminal counter + duration histogram.
// status ∈ {ok, error, cancelled, budget}; errorType is "none" on
// non-error terminals. Duration is from Run entry to terminal
// emit.
func (m *Metrics) onRunTerminal(status, errorType string, d time.Duration) {
	if m == nil {
		return
	}
	m.runTerminal.WithLabelValues(status, errorType).Inc()
	m.runDuration.WithLabelValues(status).Observe(d.Seconds())
}

// onProviderCall records one provider Stream() invocation and the
// usage it reported. status ∈ {ok, error, cancelled}. Tokens that
// the provider never surfaced arrive as 0 — no sample is recorded
// in that case, which matches the "don't invent data" principle.
func (m *Metrics) onProviderCall(model, status string, d time.Duration, promptTokens, completionTokens int64) {
	if m == nil {
		return
	}
	m.providerCalls.WithLabelValues(model, status).Inc()
	m.providerDuration.WithLabelValues(model).Observe(d.Seconds())
	if promptTokens > 0 {
		m.providerTokens.WithLabelValues(model, "prompt").Add(float64(promptTokens))
	}
	if completionTokens > 0 {
		m.providerTokens.WithLabelValues(model, "completion").Add(float64(completionTokens))
	}
}

// onToolCall records one tool invocation attempt. status ∈
// {ok, error}; errorType is "none" on ok, else the fixed enum
// {tool,panic,cancelled,other}.
func (m *Metrics) onToolCall(toolName, status, errorType string, d time.Duration) {
	if m == nil {
		return
	}
	m.toolCalls.WithLabelValues(toolName, status, errorType).Inc()
	m.toolDuration.WithLabelValues(toolName).Observe(d.Seconds())
}

// onEventlogAppend records one c.log.Append outcome. kind is
// event.Kind.String(); status ∈ {ok, error}.
func (m *Metrics) onEventlogAppend(kind, status string, d time.Duration) {
	if m == nil {
		return
	}
	m.eventlogAppends.WithLabelValues(kind, status).Inc()
	m.eventlogDuration.WithLabelValues(kind).Observe(d.Seconds())
}

// onBudgetExceeded records one budget trip by axis ∈
// {input_tokens,output_tokens,cost_usd,wall_clock}.
func (m *Metrics) onBudgetExceeded(axis string) {
	if m == nil {
		return
	}
	m.budgetExceeded.WithLabelValues(axis).Inc()
}

// stepSink returns an implementation of step.MetricsSink backed
// by this *Metrics. Nil receivers return nil so the step layer
// sees a missing sink and skips recording entirely — there is no
// trampoline overhead on the no-metrics path.
func (m *Metrics) stepSink() step.MetricsSink {
	if m == nil {
		return nil
	}
	return metricsStepSink{m: m}
}

// metricsStepSink is the thin adapter between the package-private
// *Metrics methods and the public step.MetricsSink interface.
// Pass-by-value so it's cheap to pass through step.Config.
type metricsStepSink struct{ m *Metrics }

func (s metricsStepSink) ObserveProviderCall(model, status string, d time.Duration, promptTokens, completionTokens int64) {
	s.m.onProviderCall(model, status, d, promptTokens, completionTokens)
}
func (s metricsStepSink) ObserveToolCall(toolName, status, errorType string, d time.Duration) {
	s.m.onToolCall(toolName, status, errorType, d)
}
func (s metricsStepSink) ObserveEventlogAppend(kind, status string, d time.Duration) {
	s.m.onEventlogAppend(kind, status, d)
}
func (s metricsStepSink) ObserveBudgetExceeded(axis string) {
	s.m.onBudgetExceeded(axis)
}
